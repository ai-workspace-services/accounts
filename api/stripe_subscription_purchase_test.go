package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestSubscriptionQuoteUserExpiryAndTamper(t *testing.T) {
	c := newStripeClient(StripeConfig{SecretKey: "sk_test_fixture"})
	q := subscriptionQuote{UserID: "account-1", ProrationDate: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 300}
	token := c.signSubscriptionQuote(q)
	if _, err := c.readSubscriptionQuote(token, q.UserID); err != nil {
		t.Fatal(err)
	}
	for _, entry := range [][2]string{{token, "other-account"}, {"x" + token, q.UserID}, {"malformed", q.UserID}} {
		if _, err := c.readSubscriptionQuote(entry[0], entry[1]); err == nil {
			t.Fatal("accepted invalid quote")
		}
	}
	q.ExpiresAt = time.Now().Unix() - 1
	if _, err := c.readSubscriptionQuote(c.signSubscriptionQuote(q), q.UserID); err == nil {
		t.Fatal("accepted expired quote")
	}
}

func TestMonthlyUpgradeReplacesOneItemAndPreservesUsageAfterPayment(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Upgrade", "upgrade-flow@example.invalid", "upgrade-fixture")
	plans := []*store.BillingPlan{
		{PlanID: "PLUS", StripePriceID: "price_plus", PriceAmount: 2000, PriceCurrency: "CNY", PriceUnit: "month", IncludedQuotaBytes: 20 << 30, Kind: "subscription", Active: true},
		{PlanID: "PRO-MONTHLY-V2", StripePriceID: "price_pro", PriceAmount: 5000, PriceCurrency: "CNY", PriceUnit: "month", IncludedQuotaBytes: 50 << 30, Kind: "subscription", Active: true},
	}
	for _, p := range plans {
		if err := st.UpsertBillingPlan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PLUS", ExternalID: "sub_upgrade", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	if err := h.applyPlanEntitlements(ctx, user.ID, plans[0]); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-15 * 24 * time.Hour)
	end := start.Add(30 * 24 * time.Hour)
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 14 << 30, PeriodStart: &start, PeriodEnd: &end}); err != nil {
		t.Fatal(err)
	}
	var updated, paid atomic.Bool
	var mutations atomic.Int32
	var prorate int64
	subJSON := func() string {
		price, invoice, pending := "price_plus", "in_old", "null"
		if updated.Load() {
			invoice = "in_upgrade"
			pending = `{"expires_at":9999999999}`
		}
		if paid.Load() {
			price = "price_pro"
			pending = "null"
		}
		return fmt.Sprintf(`{"id":"sub_upgrade","status":"active","customer":"cus_fixture","metadata":{"user_id":%q,"plan_id":"PLUS","kind":"subscription"},"collection_method":"charge_automatically","latest_invoice":%q,"pending_update":%s,"items":{"data":[{"id":"si_existing","quantity":1,"price":{"id":%q},"current_period_start":%d,"current_period_end":%d}]}}`, user.ID, invoice, pending, price, start.Unix(), end.Unix())
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/subscriptions/sub_upgrade":
			fmt.Fprint(w, subJSON())
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/invoices/"):
			id := strings.TrimPrefix(r.URL.Path, "/invoices/")
			status := "open"
			if id == "in_old" || paid.Load() {
				status = "paid"
			}
			fmt.Fprintf(w, `{"id":%q,"status":%q,"subscription":"sub_upgrade","hosted_invoice_url":"https://invoice.stripe.test/upgrade"}`, id, status)
		case r.Method == "POST" && r.URL.Path == "/invoices/create_preview":
			_ = r.ParseForm()
			if r.Form.Get("subscription_details[proration_date]") != "" {
				fmt.Sscan(r.Form.Get("subscription_details[proration_date]"), &prorate)
			}
			fmt.Fprint(w, `{"amount_due":1500,"total":5000,"currency":"cny"}`)
		case r.Method == "POST" && r.URL.Path == "/subscriptions/sub_upgrade":
			mutations.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("items[0][id]") != "si_existing" || r.Form.Get("payment_behavior") != "pending_if_incomplete" || r.Form.Get("proration_date") != fmt.Sprint(prorate) || r.Form.Get("proration_behavior") != "always_invoice" || r.Header.Get("Idempotency-Key") == "" {
				t.Error("unsafe upgrade request", r.Form)
			}
			updated.Store(true)
			fmt.Fprint(w, subJSON())
		default:
			t.Error("unexpected provider request", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer provider.Close()
	h.stripe = newStripeClient(StripeConfig{SecretKey: "sk_test_fixture"})
	h.stripe.apiBaseURL = provider.URL
	h.stripe.httpClient = provider.Client()
	quote, err := h.buildSubscriptionQuote(ctx, user.ID, plans[1], time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if quote.Action != "upgrade" || quote.AmountDue != 1500 || quote.RenewalAmount != 5000 || quote.RenewalAt != end.Unix() {
		t.Fatalf("bad preview %+v", quote)
	}
	req := stripeCheckoutRequest{PlanID: plans[1].PlanID, StripePriceID: plans[1].StripePriceID, Mode: "subscription", ProductSlug: "xconnect", PreviewToken: h.stripe.signSubscriptionQuote(quote)}
	execute := func() int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/checkout", nil)
		h.executeSubscriptionPurchase(c, user, req)
		return w.Code
	}
	if code := execute(); code != 200 {
		t.Fatalf("execute status %d", code)
	}
	if code := execute(); code != 409 || mutations.Load() != 1 {
		t.Fatalf("duplicate charge: code=%d mutations=%d", code, mutations.Load())
	}
	deliver := func() {
		var sub stripeSubscription
		_ = json.Unmarshal([]byte(subJSON()), &sub)
		if err := h.syncSubscriptionEntitlements(ctx, &sub, false); err != nil {
			t.Fatal(err)
		}
	}
	deliver() // Active subscription with a pending update must not grant Pro.
	quota, _ := st.GetAccountQuotaState(ctx, user.ID)
	if quota.RemainingIncludedQuota != 14<<30 {
		t.Fatal("unpaid upgrade granted quota")
	}
	paid.Store(true)
	for range 2 {
		deliver()
	}
	if pending, err := h.reconcileSubscriptionPurchase(ctx, user.ID); err != nil || pending == nil {
		t.Fatal("released before the subscription projection completed", err)
	}
	var sub stripeSubscription
	_ = json.Unmarshal([]byte(subJSON()), &sub)
	if err := h.upsertStripeSubscription(ctx, &sub, "cus_fixture"); err != nil {
		t.Fatal(err)
	}
	quota, _ = st.GetAccountQuotaState(ctx, user.ID)
	if quota.RemainingIncludedQuota != 44<<30 || !quota.PeriodStart.Equal(start) || !quota.PeriodEnd.Equal(end) {
		t.Fatalf("usage/period changed %+v", quota)
	}
	pending, err := h.reconcileSubscriptionPurchase(ctx, user.ID)
	if err != nil || pending != nil {
		t.Fatalf("paid projection did not release gate: %v %+v", err, pending)
	}
	stale := stripeEvent{Type: "customer.subscription.updated"}
	stale.Data.Object = json.RawMessage(strings.Replace(subJSON(), "price_pro", "price_plus", 1))
	if err := h.handleStripeEvent(ctx, stale); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	quota, _ = st.GetAccountQuotaState(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:PRO-MONTHLY-V2" || quota.RemainingIncludedQuota != 44<<30 {
		t.Fatal("stale webhook reverted the paid plan or reset usage")
	}
}

func TestPurchaseBlocksDuplicateLegacyAndUnsupportedChanges(t *testing.T) {
	st := store.NewMemoryStore()
	ctx := context.Background()
	u := seedActiveUser(t, st, "Existing", "existing-plan@example.invalid", "existing-proxy")
	h := &handler{store: st}
	for _, status := range []string{"past_due", "trialing", "active"} {
		if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: u.ID, ExternalID: "sub_existing", Kind: "subscription", Provider: "stripe", PlanID: "PRO-MONTHLY-V2", Status: status}); err != nil {
			t.Fatal(err)
		}
		for _, planID := range []string{"PLUS", "PRO-MONTHLY-V2", "PRO-YEARLY-V2"} {
			_, err := h.buildSubscriptionQuote(ctx, u.ID, &store.BillingPlan{PlanID: planID, Active: true, Kind: "subscription", PriceAmount: 5000, StripePriceID: "price_test"}, time.Now().Unix())
			if err == nil {
				t.Fatalf("allowed unsupported change %s -> %s", status, planID)
			}
		}
	}
}

func TestFreshPurchaseAndAmbiguousNetworkFailureKeepDurableGate(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("providerFailure=", failure), func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemoryStore()
			u := seedActiveUser(t, st, "New subscriber", "new-subscription@example.invalid", "new-proxy")
			plan := &store.BillingPlan{PlanID: "PRO-YEARLY-V2", Kind: "subscription", PriceAmount: 50000, PriceCurrency: "CNY", PriceUnit: "year", StripePriceID: "price_year", Active: true}
			if err := st.UpsertBillingPlan(ctx, plan); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: u.ID, Kind: "subscription", PlanID: store.BillingPlanFree, Status: "active", Provider: "local", ExternalID: "free"}); err != nil {
				t.Fatal(err)
			}
			var charges atomic.Int32
			var originalBody, originalKey string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "POST" && r.URL.Path == "/invoices/create_preview":
					fmt.Fprint(w, `{"amount_due":50000,"total":50000,"currency":"cny"}`)
				case r.Method == "POST" && r.URL.Path == "/checkout/sessions":
					charges.Add(1)
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					if originalBody == "" {
						originalBody = string(body)
						originalKey = r.Header.Get("Idempotency-Key")
					} else if originalBody != string(body) || originalKey != r.Header.Get("Idempotency-Key") {
						t.Error("retry changed the original request or provider key")
					}
					_ = r.ParseForm()
					if r.Form.Get("mode") != "subscription" || r.Form.Get("line_items[0][price]") != "price_year" || r.Header.Get("Idempotency-Key") == "" {
						t.Error("unsafe fresh checkout")
					}
					fmt.Fprint(w, `{"id":"cs_fresh","url":"https://checkout.stripe.test/fresh"}`)
				case r.Method == "GET" && r.URL.Path == "/checkout/sessions/cs_fresh":
					fmt.Fprintf(w, `{"id":"cs_fresh","status":"open","metadata":{"user_id":%q,"plan_id":"PRO-YEARLY-V2"}}`, u.ID)
				default:
					t.Error("unexpected request", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer provider.Close()
			h := &handler{store: st, stripe: newStripeClient(StripeConfig{SecretKey: "sk_test_fixture"})}
			h.stripe.apiBaseURL = provider.URL
			h.stripe.httpClient = provider.Client()
			if failure {
				h.stripe.httpClient.Transport = &subscriptionLostReplyTransport{base: h.stripe.httpClient.Transport}
			}
			q, err := h.buildSubscriptionQuote(ctx, u.ID, plan, time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			req := stripeCheckoutRequest{PlanID: plan.PlanID, StripePriceID: plan.StripePriceID, ProductSlug: "xconnect", Mode: "subscription", PreviewToken: h.stripe.signSubscriptionQuote(q)}
			execute := func() int {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/checkout", nil)
				h.executeSubscriptionPurchase(c, u, req)
				return w.Code
			}
			expected := 200
			if failure {
				expected = 502
			}
			if code := execute(); code != expected {
				t.Fatalf("status %d expected %d", code, expected)
			}
			if code := execute(); code != 409 || charges.Load() != 1 {
				t.Fatalf("duplicate fresh checkout: %d %d", code, charges.Load())
			}
			if failure {
				op, err := st.PendingSubscriptionPurchase(ctx, u.ID)
				if err != nil || !retryableSubscriptionPurchase(op) {
					t.Fatal("missing retryable original reservation", err)
				}
				u.Email = "changed-after-checkout@example.invalid"
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/retry", nil)
				h.submitSubscriptionPurchase(c, u, op)
				if w.Code != 200 || charges.Load() != 2 {
					t.Fatal("did not safely replay the original request", w.Code, charges.Load())
				}
				op, _ = st.PendingSubscriptionPurchase(ctx, u.ID)
				if op.ProviderOperationID != "cs_fresh" || retryableSubscriptionPurchase(op) {
					t.Fatal("did not checkpoint the original checkout")
				}
				h.stripe.secretKey = "sk_test_other_account"
				blocked := httptest.NewRecorder()
				other, _ := gin.CreateTestContext(blocked)
				other.Request = httptest.NewRequest("POST", "/retry", nil)
				h.submitSubscriptionPurchase(other, u, op)
				if blocked.Code != 409 || charges.Load() != 2 {
					t.Fatal("replayed against a changed payment authority")
				}
			}
			if profile, _ := st.GetAccountBillingProfile(ctx, u.ID); profile != nil {
				t.Fatal("granted before paid webhook")
			}
		})
	}
}

type subscriptionLostReplyTransport struct {
	base    http.RoundTripper
	dropped atomic.Bool
}

func (transport *subscriptionLostReplyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil && request.Method == "POST" && request.URL.Path == "/checkout/sessions" && transport.dropped.CompareAndSwap(false, true) {
		response.Body.Close()
		return nil, errors.New("simulated lost reply after provider acceptance")
	}
	return response, err
}

func TestSubscriptionRetryWindowNeverAllowsNewProviderOperation(t *testing.T) {
	for _, op := range []*store.FinanceOperation{nil, {CreatedAt: time.Now().Add(-21 * time.Minute)}, {CreatedAt: time.Now(), ProviderOperationID: "cs_existing"}} {
		if retryableSubscriptionPurchase(op) {
			t.Fatal("unsafe provider retry")
		}
	}
}
