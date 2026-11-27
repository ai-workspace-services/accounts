package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func signStripeWebhookForTest(secret string, payload []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(payload)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func seedStripeAdminSession(t *testing.T, st store.Store) (string, *store.User) {
	t.Helper()
	ctx := context.Background()
	admin := &store.User{
		Name:          "Stripe Ops Admin",
		Email:         "stripe-ops@example.com",
		EmailVerified: true,
		Role:          store.RoleAdmin,
		Level:         store.LevelAdmin,
		Active:        true,
		Permissions:   []string{permissionAdminSettingsRead, permissionAdminSettingsWrite},
	}
	if err := st.CreateUser(ctx, admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	token := "stripe-ops-admin-session"
	if err := st.CreateSession(ctx, token, admin.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	return token, admin
}

func seedStripeUserSession(t *testing.T, st store.Store, email, name string) (string, *store.User) {
	t.Helper()
	user := seedActiveUser(t, st, name, email, strings.Split(email, "@")[0])
	token := "user-session-" + user.ID
	if err := st.CreateSession(context.Background(), token, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	return token, user
}

func TestStripeProfilesEnvFallbackAndLoading(t *testing.T) {
	t.Setenv("STRIPE_DEFAULT_PROFILE", "")
	t.Setenv("STRIPE_PROFILES_FILE", "")
	t.Setenv("STRIPE_PROFILES_JSON", "")

	// Legacy single-profile env / StripeConfig maps to default "hk" profile.
	st := store.NewMemoryStore()
	adminToken, _ := seedStripeAdminSession(t, st)
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithStripeConfig(StripeConfig{
		SecretKey:      "sk_test_legacy_hk",
		WebhookSecret:  "whsec_legacy_hk",
		AccountID:      "acct_legacy_hk",
		MerchantEntity: "Legacy HK Ltd",
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/auth/admin/billing/stripe-profiles", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk_test_legacy_hk") || strings.Contains(body, "whsec_legacy_hk") {
		t.Fatalf("profiles response leaked secret material: %s", body)
	}
	var listed struct {
		DefaultProfile string                 `json:"defaultProfile"`
		Profiles       []stripeProfileSummary `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.DefaultProfile != "hk" || len(listed.Profiles) != 1 || listed.Profiles[0].Profile != "hk" || !listed.Profiles[0].IsDefault || !listed.Profiles[0].Enabled {
		t.Fatalf("unexpected fallback profiles payload: %+v", listed)
	}

	// JSON multi-entity configuration sets HK + US + default profile.
	t.Setenv("STRIPE_PROFILES_JSON", `{
		"defaultProfile": "us",
		"profiles": {
			"hk": {"accountId":"acct_hk_env","merchantEntity":"HK Entity Ltd","mode":"sandbox","secretKey":"sk_test_hk_env","webhookSecret":"whsec_hk_env"},
			"us": {"accountId":"acct_us_env","merchantEntity":"US Entity Inc","mode":"sandbox","secretKey":"sk_test_us_env","webhookSecret":"whsec_us_env"}
		}
	}`)
	routerMulti := gin.New()
	RegisterRoutes(routerMulti, WithStore(st), WithStripeConfig(StripeConfig{FrontendURL: "https://console.example.com"}))
	reqMulti := httptest.NewRequest(http.MethodGet, "/api/auth/admin/billing/stripe-profiles", nil)
	reqMulti.Header.Set("Authorization", "Bearer "+adminToken)
	recMulti := httptest.NewRecorder()
	routerMulti.ServeHTTP(recMulti, reqMulti)
	if recMulti.Code != http.StatusOK {
		t.Fatalf("multi status=%d body=%s", recMulti.Code, recMulti.Body.String())
	}
	if strings.Contains(recMulti.Body.String(), "sk_test_") || strings.Contains(recMulti.Body.String(), "whsec_") {
		t.Fatalf("multi profiles leaked secrets: %s", recMulti.Body.String())
	}
	if err := json.Unmarshal(recMulti.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.DefaultProfile != "us" || len(listed.Profiles) != 2 {
		t.Fatalf("unexpected multi profiles payload: %+v", listed)
	}
}

func TestMultiEntityCheckoutWebhooksPortalAndIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	adminToken, _ := seedStripeAdminSession(t, st)
	userToken, user := seedStripeUserSession(t, st, "hybrid-buyer@example.com", "Hybrid Buyer")

	var hkCheckoutCalls, hkPortalCalls, usCheckoutCalls, usPortalCalls, usCancelCalls atomic.Int32
	startUnix := time.Now().UTC().Add(-24 * time.Hour).Unix()
	endUnix := time.Now().UTC().Add(29 * 24 * time.Hour).Unix()

	hkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/invoices/create_preview":
			fmt.Fprint(w, `{"amount_due":2000,"total":2000,"currency":"hkd"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/checkout/sessions":
			hkCheckoutCalls.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("line_items[0][price]") != "price_hk_plus" || r.Form.Get("metadata[stripe_profile]") != "hk" {
				t.Errorf("unexpected HK checkout form: %v", r.Form)
			}
			fmt.Fprint(w, `{"id":"cs_hk_1","url":"https://checkout.stripe.com/c/pay/cs_hk_1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/customers":
			fmt.Fprint(w, `{"data":[{"id":"cus_hk_lookup"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/billing_portal/configurations/bpc_hk_1":
			fmt.Fprint(w, `{"id":"bpc_hk_1","active":true,"features":{"subscription_update":{"enabled":false},"subscription_cancel":{"enabled":false},"customer_update":{"enabled":false},"invoice_history":{"enabled":true},"payment_method_update":{"enabled":true}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/billing_portal/configurations/bpc_us_1":
			fmt.Fprint(w, `{"id":"bpc_us_1","active":true,"features":{"subscription_update":{"enabled":false},"subscription_cancel":{"enabled":false},"customer_update":{"enabled":false},"invoice_history":{"enabled":true},"payment_method_update":{"enabled":true}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/billing_portal/sessions":
			hkPortalCalls.Add(1)
			fmt.Fprint(w, `{"id":"bps_hk_1","url":"https://billing.stripe.com/p/session/hk"}`)
		default:
			t.Errorf("unexpected HK Stripe API call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer hkServer.Close()

	usServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/invoices/create_preview":
			fmt.Fprint(w, `{"amount_due":1000,"total":1000,"currency":"usd"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/checkout/sessions":
			usCheckoutCalls.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("line_items[0][price]") != "price_us_pro" || r.Form.Get("metadata[stripe_profile]") != "us" {
				t.Errorf("unexpected US checkout form: %v", r.Form)
			}
			fmt.Fprint(w, `{"id":"cs_us_1","url":"https://checkout.stripe.com/c/pay/cs_us_1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions/sub_us_1":
			fmt.Fprintf(w, `{"id":"sub_us_1","status":"active","customer":"cus_us_1","metadata":{"user_id":%q,"plan_id":"US-PRO-MONTHLY","kind":"subscription","stripe_profile":"us"},"collection_method":"charge_automatically","latest_invoice":"in_us_1","current_period_start":%d,"current_period_end":%d,"items":{"data":[{"id":"si_us_1","quantity":1,"price":{"id":"price_us_pro"},"current_period_start":%d,"current_period_end":%d}]}}`, user.ID, startUnix, endUnix, startUnix, endUnix)
		case r.Method == http.MethodGet && r.URL.Path == "/invoices/in_us_1":
			fmt.Fprintf(w, `{"id":"in_us_1","status":"paid","subscription":"sub_us_1","customer":"cus_us_1","currency":"usd","amount_paid":1000,"payment_intent":"pi_us_1","billing_reason":"subscription_create","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price_us_pro","period":{"start":%d,"end":%d}}]}}`, startUnix, startUnix, endUnix)
		case r.Method == http.MethodGet && r.URL.Path == "/invoices":
			fmt.Fprintf(w, `{"data":[{"id":"in_us_1","status":"paid","subscription":"sub_us_1","amount_paid":1000,"currency":"usd","payment_intent":"pi_us_1"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/billing_portal/configurations/bpc_us_1":
			fmt.Fprint(w, `{"id":"bpc_us_1","active":true,"features":{"subscription_update":{"enabled":false},"subscription_cancel":{"enabled":false},"customer_update":{"enabled":false},"invoice_history":{"enabled":true},"payment_method_update":{"enabled":true}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/billing_portal/sessions":
			usPortalCalls.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("customer") != "cus_us_1" {
				t.Errorf("expected direct cus_us_1 portal session, got customer=%q", r.Form.Get("customer"))
			}
			fmt.Fprint(w, `{"id":"bps_us_1","url":"https://billing.stripe.com/p/session/us"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions/sub_us_1":
			usCancelCalls.Add(1)
			fmt.Fprintf(w, `{"id":"sub_us_1","status":"active","cancel_at_period_end":true,"customer":"cus_us_1","metadata":{"user_id":%q},"current_period_start":%d,"current_period_end":%d,"items":{"data":[{"id":"si_us_1","quantity":1,"price":{"id":"price_us_pro"},"current_period_start":%d,"current_period_end":%d}]}}`, user.ID, startUnix, endUnix, startUnix, endUnix)
		default:
			t.Errorf("unexpected US Stripe API call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer usServer.Close()

	router := gin.New()
	var hRef *handler
	RegisterRoutes(router, WithStore(st), WithStripeProfiles("hk", map[string]StripeConfig{
		"hk": {
			Profile:               "hk",
			AccountID:             "acct_hk_live_fixture",
			MerchantEntity:        "HK Entity Ltd",
			Mode:                  "sandbox",
			SecretKey:             "sk_test_hk_entity",
			WebhookSecret:         "whsec_hk_entity",
			FrontendURL:           "https://console.example.com",
			PortalConfigurationID: "bpc_hk_1",
			APIBaseURL:            hkServer.URL,
		},
		"us": {
			Profile:               "us",
			AccountID:             "acct_us_live_fixture",
			MerchantEntity:        "US Entity Inc",
			Mode:                  "sandbox",
			SecretKey:             "sk_test_us_entity",
			WebhookSecret:         "whsec_us_entity",
			FrontendURL:           "https://console.example.com",
			PortalConfigurationID: "bpc_us_1",
			APIBaseURL:            usServer.URL,
		},
	}), func(h *handler) {
		hRef = h
	})

	// 1. Seed HK + US plans via Admin Plan API and verify persisted profile metadata.
	for _, tc := range []struct {
		planID string
		body   string
	}{
		{"PLUS", `{"displayName":"HK Plus","kind":"subscription","packageName":"plus","stripePriceId":"price_hk_plus","priceAmount":2000,"priceCurrency":"HKD","priceUnit":"month","stripeProfile":"hk","businessLine":"global_hk","merchantEntity":"HK Entity Ltd","includedQuotaBytes":32212254720,"active":true,"sortOrder":10,"reason":"seed hk"}`},
		{"US-PRO-MONTHLY", `{"displayName":"US Pro","kind":"subscription","packageName":"pro","stripePriceId":"price_us_pro","priceAmount":1000,"priceCurrency":"USD","priceUnit":"month","stripeProfile":"us","businessLine":"us_nodes","merchantEntity":"US Entity Inc","includedQuotaBytes":107374182400,"active":true,"sortOrder":20,"reason":"seed us"}`},
		{"PRO-MONTHLY-V2", `{"displayName":"HK Pro","kind":"subscription","packageName":"pro","stripePriceId":"price_hk_pro","priceAmount":5000,"priceCurrency":"HKD","priceUnit":"month","stripeProfile":"hk","businessLine":"global_hk","merchantEntity":"HK Entity Ltd","includedQuotaBytes":107374182400,"active":true,"sortOrder":15,"reason":"seed hk pro"}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/billing/plans/"+tc.planID, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("upsert %s failed: %d %s", tc.planID, rec.Code, rec.Body.String())
		}
	}

	// Public plans expose stripeProfile/businessLine/merchantEntity cleanly.
	pubReq := httptest.NewRequest(http.MethodGet, "/api/billing/plans", nil)
	pubRec := httptest.NewRecorder()
	router.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK || !strings.Contains(pubRec.Body.String(), `"stripeProfile":"us"`) || !strings.Contains(pubRec.Body.String(), `"businessLine":"us_nodes"`) {
		t.Fatalf("public plans missing multi-entity fields: %d %s", pubRec.Code, pubRec.Body.String())
	}

	// 2. Preview & checkout routing for US plan hits usServer only (then reset pending purchase so webhook test runs cleanly).
	prevReq := httptest.NewRequest(http.MethodPost, "/api/auth/stripe/subscription/preview", strings.NewReader(`{"planId":"US-PRO-MONTHLY","stripePriceId":"price_us_pro","mode":"subscription","productSlug":"xconnect"}`))
	prevReq.Header.Set("Content-Type", "application/json")
	prevReq.Header.Set("Authorization", "Bearer "+userToken)
	prevRec := httptest.NewRecorder()
	router.ServeHTTP(prevRec, prevReq)
	if prevRec.Code != http.StatusOK {
		t.Fatalf("US preview failed: %d %s", prevRec.Code, prevRec.Body.String())
	}
	var previewResp struct {
		PreviewToken string `json:"previewToken"`
	}
	_ = json.Unmarshal(prevRec.Body.Bytes(), &previewResp)

	chkReq := httptest.NewRequest(http.MethodPost, "/api/auth/stripe/checkout", strings.NewReader(fmt.Sprintf(`{"planId":"US-PRO-MONTHLY","stripePriceId":"price_us_pro","mode":"subscription","productSlug":"xconnect","previewToken":%q}`, previewResp.PreviewToken)))
	chkReq.Header.Set("Content-Type", "application/json")
	chkReq.Header.Set("Authorization", "Bearer "+userToken)
	chkRec := httptest.NewRecorder()
	router.ServeHTTP(chkRec, chkReq)
	if chkRec.Code != http.StatusOK || usCheckoutCalls.Load() != 1 || hkCheckoutCalls.Load() != 0 {
		t.Fatalf("US checkout failed: code=%d us=%d hk=%d body=%s", chkRec.Code, usCheckoutCalls.Load(), hkCheckoutCalls.Load(), chkRec.Body.String())
	}

	// 3. Profile-scoped webhook isolation:
	// - unconfigured profile -> 503
	euReq := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook/eu", strings.NewReader(`{}`))
	euRec := httptest.NewRecorder()
	router.ServeHTTP(euRec, euReq)
	if euRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unconfigured profile webhook, got %d", euRec.Code)
	}

	// - US event signed with HK secret -> 401
	eventPayload := []byte(`{"id":"evt_shared_001","type":"customer.subscription.created","livemode":false,"data":{"object":{"id":"sub_us_1"}}}`)
	wrongSigReq := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook/us", bytes.NewReader(eventPayload))
	wrongSigReq.Header.Set("Stripe-Signature", signStripeWebhookForTest("whsec_hk_entity", eventPayload))
	wrongSigRec := httptest.NewRecorder()
	router.ServeHTTP(wrongSigRec, wrongSigReq)
	if wrongSigRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for cross-profile webhook signature, got %d %s", wrongSigRec.Code, wrongSigRec.Body.String())
	}

	// - US event signed with US secret -> 200, persists stripe_profile="us" and key "us:evt_shared_001"
	usSigReq := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook/us", bytes.NewReader(eventPayload))
	usSigReq.Header.Set("Stripe-Signature", signStripeWebhookForTest("whsec_us_entity", eventPayload))
	usSigRec := httptest.NewRecorder()
	router.ServeHTTP(usSigRec, usSigReq)
	if usSigRec.Code != http.StatusOK {
		t.Fatalf("US webhook failed: %d %s", usSigRec.Code, usSigRec.Body.String())
	}

	subs, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil || len(subs) == 0 {
		t.Fatalf("expected persisted US subscription, err=%v len=%d", err, len(subs))
	}
	if subs[0].Meta["stripe_profile"] != "us" || subs[0].Meta["stripe_account_id"] != "acct_us_live_fixture" {
		t.Fatalf("expected stripe_profile=us metadata, got %+v", subs[0].Meta)
	}

	// Same event ID sent to HK webhook (different entity, no key collision because US used "us:evt_shared_001"):
	// Verify it is NOT treated as duplicate of US event (uses refund.created so no subscription fetch needed).
	hkDupCheckPayload := []byte(`{"id":"evt_shared_001","type":"refund.created","livemode":false,"data":{"object":{"id":"re_hk_ignore","amount":100,"currency":"hkd","created":1700000000,"payment_intent":"pi_nonexistent","status":"pending"}}}`)
	hkEventReq := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook/hk", bytes.NewReader(hkDupCheckPayload))
	hkEventReq.Header.Set("Stripe-Signature", signStripeWebhookForTest("whsec_hk_entity", hkDupCheckPayload))
	hkEventRec := httptest.NewRecorder()
	router.ServeHTTP(hkEventRec, hkEventReq)
	if hkEventRec.Code != http.StatusOK || strings.Contains(hkEventRec.Body.String(), `"duplicate":true`) {
		t.Fatalf("expected independent event tracking per profile, got code=%d body=%s", hkEventRec.Code, hkEventRec.Body.String())
	}

	// 4. Cross-entity upgrade & renewal prevention: user owns active US sub, tries to upgrade/renew to HK plan -> rejected.
	if _, err := hRef.buildSubscriptionQuote(ctx, user.ID, &store.BillingPlan{PlanID: "PRO-MONTHLY-V2", StripePriceID: "price_hk_pro", PriceAmount: 5000, PriceCurrency: "HKD", PriceUnit: "month", Kind: "subscription", PackageName: "pro", Active: true, Features: map[string]any{"stripe_profile": "hk"}}, time.Now().Unix()); err == nil || !strings.Contains(err.Error(), "different Stripe entities") {
		t.Fatalf("expected cross-entity upgrade rejection, got %v", err)
	}
	if _, err := hRef.renewalQuote(ctx, user.ID, "PLUS", time.Now().Unix()); err == nil || !strings.Contains(err.Error(), "different Stripe entities") {
		t.Fatalf("expected cross-entity renewal rejection, got %v", err)
	}

	// 5. Portal routing:
	// - Auto-routed by user's active US subscription (without hitting HK server!)
	portalAutoReq := httptest.NewRequest(http.MethodPost, "/api/auth/stripe/portal", strings.NewReader(`{"returnPath":"/panel/subscription"}`))
	portalAutoReq.Header.Set("Content-Type", "application/json")
	portalAutoReq.Header.Set("Authorization", "Bearer "+userToken)
	portalAutoRec := httptest.NewRecorder()
	router.ServeHTTP(portalAutoRec, portalAutoReq)
	if portalAutoRec.Code != http.StatusOK || usPortalCalls.Load() != 1 || hkPortalCalls.Load() != 0 {
		t.Fatalf("auto portal routing failed: code=%d us=%d hk=%d body=%s", portalAutoRec.Code, usPortalCalls.Load(), hkPortalCalls.Load(), portalAutoRec.Body.String())
	}

	// - Explicit override routes to HK when requested
	portalHKReq := httptest.NewRequest(http.MethodPost, "/api/auth/stripe/portal", strings.NewReader(`{"stripeProfile":"hk"}`))
	portalHKReq.Header.Set("Content-Type", "application/json")
	portalHKReq.Header.Set("Authorization", "Bearer "+userToken)
	portalHKRec := httptest.NewRecorder()
	router.ServeHTTP(portalHKRec, portalHKReq)
	if portalHKRec.Code != http.StatusOK || hkPortalCalls.Load() != 1 {
		t.Fatalf("explicit HK portal routing failed: code=%d hk=%d body=%s", portalHKRec.Code, hkPortalCalls.Load(), portalHKRec.Body.String())
	}

	// 6. Settled US invoice.paid webhook populates local payment ledger, then refund review & cancellation route exclusively to US server.
	invoicePaidPayload := []byte(fmt.Sprintf(`{"id":"evt_us_invoice_paid_1","type":"invoice.paid","livemode":false,"data":{"object":{"id":"in_us_1","status":"paid","subscription":"sub_us_1","customer":"cus_us_1","currency":"usd","amount_paid":1000,"payment_intent":"pi_us_1","billing_reason":"subscription_create","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price_us_pro","period":{"start":%d,"end":%d}}]}}}}`, startUnix, startUnix, endUnix))
	usInvReq := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook/us", bytes.NewReader(invoicePaidPayload))
	usInvReq.Header.Set("Stripe-Signature", signStripeWebhookForTest("whsec_us_entity", invoicePaidPayload))
	usInvRec := httptest.NewRecorder()
	router.ServeHTTP(usInvRec, usInvReq)
	if usInvRec.Code != http.StatusOK {
		t.Fatalf("US invoice.paid webhook failed: %d %s", usInvRec.Code, usInvRec.Body.String())
	}
	if op, opErr := st.PendingSubscriptionPurchase(ctx, user.ID); opErr == nil && op != nil {
		if err := st.UpdateSubscriptionPurchase(ctx, op.ID, op.ProviderOperationID, json.RawMessage(`{"outcome":"webhook_confirmed"}`), true); err != nil {
			t.Fatalf("close pending purchase op: %v", err)
		}
	}
	reviewIntent := refundReviewRequest{AccountID: user.ID, SubscriptionID: subs[0].ID, ExternalID: "sub_us_1", StripeProfile: "us", RequestedAt: time.Now().UTC()}
	if err := hRef.collectRefundReview(ctx, &reviewIntent); err != nil {
		t.Fatalf("collectRefundReview routed to US failed: %v", err)
	}
	if err := hRef.cancelStripeSubscription(ctx, &subs[0]); err != nil {
		t.Fatalf("cancelStripeSubscription routed to US failed: %v", err)
	}
	if usCancelCalls.Load() != 1 {
		t.Fatalf("expected 1 US cancel call, got %d", usCancelCalls.Load())
	}
}
