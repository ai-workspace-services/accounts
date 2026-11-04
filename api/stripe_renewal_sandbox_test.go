package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Explicit opt-in only: creates disposable Stripe test-clock fixtures, never
// connects to a database, and uses the same pinned API version as the service.
// Inject credentials into the process environment from a secret manager; do not
// put them in shell arguments, test output, files or CI artifacts.
func TestStripeRenewalSandbox(t *testing.T) {
	if os.Getenv("ACCOUNTS_STRIPE_SANDBOX_TEST") != "1" {
		t.Skip("opt-in Stripe sandbox acceptance")
	}
	key, secret := os.Getenv("SANDBOX_STRIPE_SECRET_KEY"), os.Getenv("SANDBOX_STRIPE_WEBHOOK_SECRET")
	if !strings.HasPrefix(key, "sk_test_") || secret == "" {
		t.Fatal("test-mode key and sandbox webhook secret required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	sc := newStripeClient(StripeConfig{Mode: "sandbox", SecretKey: key, WebhookSecret: secret})
	sc.httpClient = &http.Client{Transport: sandboxStripeTransport{}, Timeout: 30 * time.Second}
	var balance struct {
		Live *bool `json:"livemode"`
	}
	if err := sc.doJSON(ctx, http.MethodGet, "/balance", &balance); err != nil {
		t.Fatalf("read-only Stripe authentication preflight failed: %v", err)
	}
	if balance.Live == nil || *balance.Live {
		t.Fatal("provider did not confirm test mode; no fixtures created")
	}
	run := "renewal-acceptance-" + uuid.NewString()
	var product struct{ ID string }
	if err := sc.doForm(ctx, http.MethodPost, "/products", url.Values{"name": {run}, "metadata[acceptance_run]": {run}}, &product); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sc.doForm(context.Background(), http.MethodPost, "/products/"+product.ID, url.Values{"active": {"false"}}, nil); err != nil {
			t.Error("fixture product cleanup failed", err)
		}
	})
	makePrice := func(amount string) string {
		var price struct{ ID string }
		if err := sc.doForm(ctx, http.MethodPost, "/prices", url.Values{"product": {product.ID}, "currency": {"usd"}, "unit_amount": {amount}, "recurring[interval]": {"month"}, "metadata[acceptance_run]": {run}}, &price); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := sc.doForm(context.Background(), http.MethodPost, "/prices/"+price.ID, url.Values{"active": {"false"}}, nil); err != nil {
				t.Error("fixture price cleanup failed", err)
			}
		})
		return price.ID
	}
	oldPrice, newPrice := makePrice("1000"), makePrice("2000")

	for _, scenario := range []string{"paid-renewal", "cancel-before-renewal"} {
		t.Run(scenario, func(t *testing.T) {
			st := store.NewMemoryStore()
			user := seedActiveUser(t, st, "Renewal acceptance", uuid.NewString()+"@example.invalid", uuid.NewString())
			for _, p := range []*store.BillingPlan{
				{PlanID: "RENEWAL-OLD", StripePriceID: oldPrice, PriceAmount: 1000, IncludedQuotaBytes: 20 << 30, PriceCurrency: "USD", PriceUnit: "month", Kind: "subscription", Active: true},
				{PlanID: "RENEWAL-NEW", StripePriceID: newPrice, PriceAmount: 2000, IncludedQuotaBytes: 50 << 30, PriceCurrency: "USD", PriceUnit: "month", Kind: "subscription", Active: true},
			} {
				if err := st.UpsertBillingPlan(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			var clock struct{ ID, Status string }
			if err := sc.doForm(ctx, http.MethodPost, "/test_helpers/test_clocks", url.Values{"name": {run}, "frozen_time": {strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)}}, &clock); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := sc.doForm(context.Background(), http.MethodDelete, "/test_helpers/test_clocks/"+clock.ID, nil, nil); err != nil {
					t.Error("fixture clock/customer/subscription cleanup failed", err)
				}
			})
			var customer struct{ ID string }
			if err := sc.doForm(ctx, http.MethodPost, "/customers", url.Values{"test_clock": {clock.ID}, "email": {user.Email}, "payment_method": {"pm_card_visa"}, "invoice_settings[default_payment_method]": {"pm_card_visa"}, "metadata[acceptance_run]": {run}}, &customer); err != nil {
				t.Fatal(err)
			}
			var sub stripeSubscription
			if err := sc.doForm(ctx, http.MethodPost, "/subscriptions", url.Values{"customer": {customer.ID}, "items[0][price]": {oldPrice}, "metadata[user_id]": {user.ID}, "metadata[plan_id]": {"RENEWAL-OLD"}, "metadata[kind]": {"subscription"}, "metadata[acceptance_run]": {run}}, &sub); err != nil {
				t.Fatal(err)
			}
			if sub.Status != "active" {
				t.Fatal("test subscription was not paid and activated")
			}
			var h *handler
			router := gin.New()
			RegisterRoutes(router, WithStore(st), func(x *handler) { x.stripe = sc; h = x })
			if err := h.upsertStripeSubscription(ctx, &sub, customer.ID); err != nil {
				t.Fatal(err)
			}
			if err := h.syncSubscriptionEntitlements(ctx, &sub, true); err != nil {
				t.Fatal(err)
			}
			token := uuid.NewString()
			if err := st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			request := func(method, path string, body any) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(body)
				r := httptest.NewRequest(method, path, bytes.NewReader(raw))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			preview := request("POST", "/api/auth/stripe/subscription/renewal/preview", map[string]string{"planId": "RENEWAL-NEW"})
			var quote struct {
				Token      string `json:"previewToken"`
				AmountDue  int64  `json:"amountDue"`
				Effective  int64  `json:"effectiveAt"`
				NextAmount int64  `json:"renewalAmount"`
			}
			if preview.Code != 200 || json.Unmarshal(preview.Body.Bytes(), &quote) != nil || quote.Token == "" || quote.AmountDue != 0 || quote.NextAmount != 2000 {
				t.Fatalf("renewal preview failed: status %d", preview.Code)
			}
			confirm := map[string]string{"previewToken": quote.Token}
			applied := request("POST", "/api/auth/stripe/subscription/renewal", confirm)
			if applied.Code != 200 {
				t.Fatalf("renewal confirmation failed: status %d", applied.Code)
			}
			replayed := request("POST", "/api/auth/stripe/subscription/renewal", confirm)
			if replayed.Code != 200 || replayed.Body.String() != applied.Body.String() {
				t.Fatal("confirmation replay did not return the original receipt")
			}
			current, err := sc.fetchSubscription(ctx, sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := st.GetAccountBillingProfile(ctx, user.ID)
			if err != nil || profile == nil || profile.IncludedQuotaBytes != 20<<30 || subscriptionPriceID(current) != oldPrice || customerIDFromAny(current.LatestInvoice) != customerIDFromAny(sub.LatestInvoice) {
				t.Fatal("scheduling changed the paid price, invoice or current entitlement")
			}
			if scenario == "cancel-before-renewal" {
				local, err := h.currentPurchaseSubscription(ctx, user.ID)
				if err != nil || local == nil {
					t.Fatal("local paid subscription unavailable")
				}
				if err := h.cancelStripeSubscription(ctx, local); err != nil {
					t.Fatal(err)
				}
				current, err = sc.fetchSubscription(ctx, sub.ID)
				if err != nil || !current.CancelAtPeriodEnd || customerIDFromAny(current.Schedule) != "" || subscriptionPriceID(current) != oldPrice {
					t.Fatal("cancel failed to release our schedule and preserve the paid term")
				}
				return
			}
			if err := sc.doForm(ctx, http.MethodPost, "/test_helpers/test_clocks/"+clock.ID+"/advance", url.Values{"frozen_time": {strconv.FormatInt(quote.Effective+3600, 10)}}, &clock); err != nil {
				t.Fatal(err)
			}
			for {
				if err := sc.doJSON(ctx, "GET", "/test_helpers/test_clocks/"+clock.ID, &clock); err != nil {
					t.Fatal(err)
				}
				if clock.Status == "ready" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("test clock did not finish advancing")
				case <-time.After(time.Second):
				}
			}
			current, err = sc.fetchSubscription(ctx, sub.ID)
			if err != nil || subscriptionPriceID(current) != newPrice {
				t.Fatal("renewal did not switch to the scheduled price")
			}
			invoice, err := sc.fetchInvoice(ctx, customerIDFromAny(current.LatestInvoice))
			if err != nil || invoice.Status != "paid" || invoice.AmountPaid != 2000 || invoice.ID == customerIDFromAny(sub.LatestInvoice) {
				t.Fatal("renewal invoice did not settle at the new price")
			}
			// Replay the real provider object through the local signed webhook
			// handler. This does not claim external webhook delivery acceptance.
			body, _ := json.Marshal(map[string]any{"id": "evt_acceptance_" + uuid.NewString(), "type": "customer.subscription.updated", "livemode": false, "data": map[string]any{"object": current}})
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(ts + "." + string(body)))
			for range 2 {
				r := httptest.NewRequest("POST", "/api/billing/stripe/webhook", bytes.NewReader(body))
				r.Header.Set("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("signed renewal webhook replay failed: status %d", w.Code)
				}
			}
			profile, err = st.GetAccountBillingProfile(ctx, user.ID)
			local, localErr := h.currentPurchaseSubscription(ctx, user.ID)
			if err != nil || localErr != nil || profile == nil || profile.IncludedQuotaBytes != 50<<30 || local == nil || local.PlanID != "RENEWAL-NEW" {
				t.Fatal("paid renewal did not update the plan and allowance")
			}
			t.Log("test clock verified renewal price, paid invoice and signed local webhook replay")
		})
	}
}

// Strip provider error messages (which can echo credentials) before test errors
// reach stdout. Restrict credentials and every request to the real test API.
type sandboxStripeTransport struct{}

func (sandboxStripeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "api.stripe.com" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk_test_") || r.Header.Get("Stripe-Version") != stripeAPIVersion {
		return nil, fmt.Errorf("sandbox request rejected")
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	defer resp.Body.Close()
	var v struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	raw, _ := json.Marshal(v)
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	return resp, nil
}
