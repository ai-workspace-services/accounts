package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/store"
)

func TestRenewalScheduleKeepsCurrentPriceUntilBoundary(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "renewal", "renewal@example.invalid", "renewal-test")
	plan := &store.BillingPlan{PlanID: "NEW", StripePriceID: "price_new", PriceAmount: 5000, PriceCurrency: "CNY", PriceUnit: "month", Kind: "subscription", Active: true, IncludedQuotaBytes: 50 << 30}
	if err := st.UpsertBillingPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "OLD", ExternalID: "sub_existing", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).Unix()
	end := time.Now().Add(24 * time.Hour).Unix()
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /subscriptions/sub_existing":
			fmt.Fprintf(w, `{"id":"sub_existing","metadata":{"user_id":%q},"status":"active","collection_method":"charge_automatically","latest_invoice":"in_paid","current_period_start":%d,"current_period_end":%d,"items":{"data":[{"id":"si_existing","quantity":1,"price":{"id":"price_old"}}]}}`, user.ID, start, end)
		case "GET /invoices/in_paid":
			fmt.Fprint(w, `{"id":"in_paid","status":"paid","subscription":"sub_existing","currency":"cny"}`)
		case "GET /prices/price_new":
			fmt.Fprint(w, `{"id":"price_new","active":true,"currency":"cny","unit_amount":5000,"billing_scheme":"per_unit","recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}}`)
		case "POST /subscription_schedules":
			_ = r.ParseForm()
			if r.Form.Get("from_subscription") != "sub_existing" || r.Header.Get("Idempotency-Key") != "operation:create" {
				t.Error("schedule not attached idempotently")
			}
			calls++
			fmt.Fprintf(w, `{"id":"sub_sched_fixture","subscription":"sub_existing","phases":[{"start_date":%d,"end_date":%d,"items":[{"price":"price_old","quantity":1}]}]}`, start, end)
		case "POST /subscription_schedules/sub_sched_fixture":
			_ = r.ParseForm()
			if r.Form.Get("phases[0][items][0][price]") != "price_old" || r.Form.Get("phases[1][items][0][price]") != "price_new" || r.Form.Get("phases[0][end_date]") != fmt.Sprint(end) || r.Form.Get("phases[1][start_date]") != fmt.Sprint(end) || r.Form.Get("proration_behavior") != "none" || r.Form.Get("phases[1][proration_behavior]") != "none" || r.Form.Get("end_behavior") != "release" {
				t.Error("schedule did not preserve paid phase")
			}
			if r.Header.Get("Idempotency-Key") != "operation:update" {
				t.Error("schedule update missing idempotency")
			}
			calls++
			_ = json.NewEncoder(w).Encode(renewalSchedule{ID: "sub_sched_fixture", Subscription: "sub_existing", Status: "active", Metadata: map[string]string{"portal_renewal_user": user.ID, "portal_renewal_key": "operation"}, Phases: []renewalPhase{
				{Start: start, End: end, Items: []struct {
					Price    string `json:"price"`
					Quantity int64  `json:"quantity"`
				}{{"price_old", 1}}},
				{Start: end, End: end + 30*86400, Items: []struct {
					Price    string `json:"price"`
					Quantity int64  `json:"quantity"`
				}{{"price_new", 1}}},
			}})
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer provider.Close()
	h := &handler{store: st, stripe: newStripeClient(StripeConfig{SecretKey: "sk_test_fixture", APIBaseURL: provider.URL})}
	q, err := h.renewalQuote(ctx, user.ID, "NEW", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountDue != 0 || q.RenewalAt != end || q.RenewalAmount != 5000 {
		t.Fatalf("bad renewal quote: %+v", q)
	}
	if _, err = h.stripe.scheduleRenewal(ctx, q, "operation"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("provider mutation count %d", calls)
	}
	if _, err = st.GetAccountBillingProfile(ctx, user.ID); err == nil {
		t.Fatal("scheduling granted an entitlement")
	}
}
