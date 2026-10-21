package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type isolatedStripeStore struct {
	store.Store
	store.NetworkTeamStore
	failNextFinish bool
}

func (s *isolatedStripeStore) FinishStripeWebhookEvent(ctx context.Context, id string, procErr error) error {
	if s.failNextFinish {
		s.failNextFinish = false
		return errors.New("isolated finalization fault")
	}
	return s.Store.FinishStripeWebhookEvent(ctx, id, procErr)
}

// Runs only in a disposable local database containing schema and the four
// catalog rows. Provider calls use a loopback fixture: this is API/database
// recovery coverage, not a real Stripe payment or an Agent socket test.
func TestStripeIsolatedCopyRecovery(t *testing.T) {
	dsn := os.Getenv("STRIPE_ISOLATED_COPY_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable local stripe_acceptance_copy database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || cfg.Database != "stripe_acceptance_copy" {
		t.Fatal("only the isolated local copy is accepted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var users int
	if err = db.QueryRow("SELECT count(*) FROM users").Scan(&users); err != nil || users != 0 {
		t.Fatal("copy must contain no original users")
	}
	ctx := context.Background()
	base, closeStore, err := store.New(ctx, store.Config{Driver: "postgres", DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(ctx)
	for _, planID := range []string{"PLUS", "PRO-MONTHLY-V2", "PRO-YEARLY-V2"} {
		t.Run(planID, func(t *testing.T) {
			plan, err := base.GetBillingPlan(ctx, planID)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Active || plan.StripePriceID == "" {
				t.Fatal("copied catalog is not configured")
			}
			st := &isolatedStripeStore{Store: base, NetworkTeamStore: base.(store.NetworkTeamStore)}
			user := seedActiveUser(t, st, "Isolated "+planID, strings.ToLower(planID)+"@example.invalid", uuid.NewString())
			user.Groups = []string{store.MonthlyFreeQuotaLimitGroup}
			if err = st.UpdateUser(ctx, user); err != nil {
				t.Fatal(err)
			}
			token := "isolated-" + uuid.NewString()
			if err = st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			free, err := st.GetBillingPlan(ctx, "FREE")
			if err != nil {
				t.Fatal(err)
			}
			h := &handler{store: st}
			if err = h.applyPlanEntitlements(ctx, user.ID, free); err != nil {
				t.Fatal(err)
			}
			freeStart, freeEnd := naturalMonthPeriod(time.Now())
			start := time.Now().UTC().Truncate(time.Second)
			end := start.AddDate(0, 1, 0)
			if plan.PriceUnit == "year" {
				end = start.AddDate(1, 0, 0)
			}
			if err = st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 0, PeriodStart: &freeStart, PeriodEnd: &freeEnd, SuspendState: "active", ThrottleState: "normal", ProxyAccessState: "active"}); err != nil {
				t.Fatal(err)
			}
			suffix := strings.ToLower(planID)
			subID, invoiceID := "sub_isolated_"+suffix, "in_isolated_"+suffix
			paid := false
			makeInvoice := func() map[string]any {
				status := "open"
				amount := int64(0)
				if paid {
					status = "paid"
					amount = plan.PriceAmount
				}
				return map[string]any{"id": invoiceID, "subscription": subID, "customer": "cus_isolated_" + suffix, "status": status, "billing_reason": "subscription_create", "currency": strings.ToLower(plan.PriceCurrency), "amount_paid": amount, "created": start.Unix(), "payment_intent": "pi_isolated_" + suffix, "status_transitions": map[string]any{"paid_at": start.Unix()}, "lines": map[string]any{"data": []any{map[string]any{"price": map[string]any{"id": plan.StripePriceID}, "period": map[string]any{"start": start.Unix(), "end": end.Unix()}}}, "has_more": false}}
			}
			subscription := map[string]any{"id": subID, "status": "active", "customer": "cus_isolated_" + suffix, "latest_invoice": invoiceID, "metadata": map[string]string{"user_id": user.ID, "plan_id": planID, "kind": "subscription", "product_slug": "xconnect"}, "current_period_start": start.Unix(), "current_period_end": end.Unix(), "items": map[string]any{"data": []any{map[string]any{"price": map[string]any{"id": plan.StripePriceID}}}}}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected provider write")
					http.Error(w, "forbidden", 500)
					return
				}
				switch r.URL.Path {
				case "/subscriptions/" + subID:
					json.NewEncoder(w).Encode(subscription)
				case "/invoices/" + invoiceID:
					json.NewEncoder(w).Encode(makeInvoice())
				case "/invoices":
					json.NewEncoder(w).Encode(map[string]any{"data": []any{makeInvoice()}, "has_more": false})
				default:
					t.Errorf("unexpected provider path: %s", r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer provider.Close()
			newRouter := func() *gin.Engine {
				router := gin.New()
				RegisterRoutes(router, WithStore(st), WithStripeConfig(StripeConfig{Mode: "sandbox", SecretKey: "sk_test_fixture", WebhookSecret: testStripeWebhookSecret}), func(x *handler) { x.stripe.apiBaseURL = provider.URL; x.stripe.httpClient = provider.Client(); h = x })
				return router
			}
			router := newRouter()
			nodesStatus := func() int {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("GET", "/api/agent-server/v1/nodes", nil)
				c.Request.Header.Set("Authorization", "Bearer "+token)
				h.listAgentNodes(c)
				return w.Code
			}
			if got := nodesStatus(); got != 403 {
				t.Fatalf("exhausted Free nodes status=%d", got)
			}
			event := func(id, kind string, obj any, live bool) []byte {
				body, e := json.Marshal(map[string]any{"id": id, "type": kind, "livemode": live, "data": map[string]any{"object": obj}})
				if e != nil {
					t.Fatal(e)
				}
				return body
			}
			checkout := map[string]any{"id": "cs_test_isolated_" + suffix, "subscription": subID, "customer": "cus_isolated_" + suffix}
			if rec := postStripeEvent(t, router, event("evt_isolated_wrong_"+suffix, "checkout.session.completed", checkout, true)); rec.Code != 400 {
				t.Fatalf("mismatched event status=%d", rec.Code)
			}
			unpaid := event("evt_isolated_unpaid_"+suffix, "customer.subscription.updated", subscription, false)
			if rec := postStripeEvent(t, router, unpaid); rec.Code != 200 {
				t.Fatalf("unpaid event status=%d %s", rec.Code, rec.Body.String())
			}
			if q, e := st.GetAccountQuotaState(ctx, user.ID); e != nil || q.RemainingIncludedQuota != 0 {
				t.Fatalf("unpaid allowance changed: %v", e)
			}
			paid = true
			completed := event("evt_isolated_checkout_"+suffix, "checkout.session.completed", checkout, false)
			if rec := postStripeEvent(t, router, completed); rec.Code != 200 {
				t.Fatalf("paid checkout status=%d %s", rec.Code, rec.Body.String())
			}
			q, e := st.GetAccountQuotaState(ctx, user.ID)
			if e != nil || q.RemainingIncludedQuota != plan.IncludedQuotaBytes {
				t.Fatalf("full allowance missing: %v", e)
			}
			if got := nodesStatus(); got != 200 {
				t.Fatalf("restored nodes status=%d", got)
			}
			q.RemainingIncludedQuota -= 3 << 30
			if err = st.UpsertAccountQuotaState(ctx, q); err != nil {
				t.Fatal(err)
			}
			settled := event("evt_isolated_paid_"+suffix, "invoice.paid", makeInvoice(), false)
			st.failNextFinish = true
			if rec := postStripeEvent(t, router, settled); rec.Code != 503 {
				t.Fatalf("lost finalization status=%d %s", rec.Code, rec.Body.String())
			}
			// Construct a new handler over the same PostgreSQL store to exercise durable
			// recovery independently of the original handler's process memory.
			router = newRouter()
			if rec := postStripeEvent(t, router, settled); rec.Code != 200 {
				t.Fatalf("recovery status=%d %s", rec.Code, rec.Body.String())
			}
			for _, payload := range [][]byte{completed, settled} {
				rec := postStripeEvent(t, router, payload)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
					t.Fatal("settled delivery was not deduplicated")
				}
			}
			q, e = st.GetAccountQuotaState(ctx, user.ID)
			if e != nil || q.RemainingIncludedQuota != plan.IncludedQuotaBytes-(3<<30) {
				t.Fatalf("replay replenished usage: %v", e)
			}
			invoices, e := st.ListFinanceInvoices(ctx, user.ID, "", 0)
			if e != nil || len(invoices) != 1 || invoices[0].AmountMinor != plan.PriceAmount {
				t.Fatalf("invoice facts: %v count=%d", e, len(invoices))
			}
			payments, e := st.ListFinancePayments(ctx, user.ID, 0)
			if e != nil || len(payments) != 1 || payments[0].AmountMinor != plan.PriceAmount {
				t.Fatalf("payment facts: %v count=%d", e, len(payments))
			}
			t.Logf("local copied-schema %s: wrong mode rejected, unpaid blocked, full paid quota, nodes403->200, finish503/retry200, replay preserves consumption and one invoice/payment", planID)
		})
	}
}
