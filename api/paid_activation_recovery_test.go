package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestExhaustedFreeStartsFullPaidAllowanceAndRecoversOnce(t *testing.T) {
	for _, samePeriod := range []bool{false, true} {
		for _, created := range []bool{false, true} {
			t.Run(fmt.Sprintf("same_period=%t/created_event=%t", samePeriod, created), func(t *testing.T) {
				ctx := context.Background()
				st := store.NewMemoryStore()
				u := seedActiveUser(t, st, "Activation", "activation@example.invalid", "activation-proxy")
				u.Groups = []string{store.MonthlyFreeQuotaLimitGroup, "operator:preserved"}
				if err := st.UpdateUser(ctx, u); err != nil {
					t.Fatal(err)
				}
				h := &handler{store: st}
				if err := st.CreateSession(ctx, "activation-session", u.ID, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				getNodes := func() int {
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("GET", "/api/agent-server/v1/nodes", nil)
					c.Request.Header.Set("Authorization", "Bearer activation-session")
					h.listAgentNodes(c)
					return w.Code
				}
				free := &store.BillingPlan{PlanID: "FREE", PackageName: "free", IncludedQuotaBytes: 5 << 30}
				plus := &store.BillingPlan{PlanID: "PLUS", PackageName: "plus", StripePriceID: "price_activation", IncludedQuotaBytes: 20 << 30, PriceUnit: "month", Active: true}
				if err := st.UpsertBillingPlan(ctx, plus); err != nil {
					t.Fatal(err)
				}
				if err := h.applyPlanEntitlements(ctx, u.ID, free); err != nil {
					t.Fatal(err)
				}
				start, end := naturalMonthPeriod(time.Now())
				if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 0, PeriodStart: &start, PeriodEnd: &end, SuspendState: "active", ThrottleState: "normal", ProxyAccessState: "active"}); err != nil {
					t.Fatal(err)
				}
				if status := getNodes(); status != http.StatusForbidden {
					t.Fatalf("exhausted Free must deny nodes: %d", status)
				}
				if !samePeriod {
					start = time.Now().UTC().Truncate(time.Second)
					end = start.AddDate(0, 1, 0)
				}
				sub := &stripeSubscription{ID: "sub_activation", Status: "active", LatestInvoice: "in_activation", Metadata: map[string]string{"user_id": u.ID, "plan_id": "PLUS"}, CurrentPeriodStart: start.Unix(), CurrentPeriodEnd: end.Unix()}
				paid := false
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/invoices/in_activation" {
						t.Errorf("unexpected provider request: %s", r.URL.Path)
						http.Error(w, "unexpected", 500)
						return
					}
					status := "open"
					if paid {
						status = "paid"
					}
					fmt.Fprintf(w, `{"id":"in_activation","subscription":"sub_activation","status":%q,"billing_reason":"subscription_create"}`, status)
				}))
				defer provider.Close()
				h.stripe = newStripeClient(StripeConfig{SecretKey: "sk_test_fixture"})
				h.stripe.apiBaseURL, h.stripe.httpClient = provider.URL, provider.Client()
				if err := h.syncSubscriptionEntitlements(ctx, sub, created); err != nil {
					t.Fatal(err)
				}
				q, _ := st.GetAccountQuotaState(ctx, u.ID)
				if q.RemainingIncludedQuota != 0 {
					t.Fatal("unpaid subscription granted access")
				}
				paid = true
				if err := h.upsertStripeSubscription(ctx, sub, "cus_activation"); err != nil {
					t.Fatal(err)
				}
				if err := h.syncSubscriptionEntitlements(ctx, sub, created); err != nil {
					t.Fatal(err)
				}
				q, _ = st.GetAccountQuotaState(ctx, u.ID)
				if q.RemainingIncludedQuota != 20<<30 {
					t.Fatalf("first paid subscription must grant 20 GiB, got %d", q.RemainingIncludedQuota)
				}
				u, _ = st.GetUserByID(ctx, u.ID)
				if exhausted, err := h.quotaExhaustedForUser(ctx, u); err != nil || exhausted {
					t.Fatalf("paid allowance remains exhausted: %v %v", exhausted, err)
				}
				if blocked, err := h.proxyAccountBlocked(ctx, u); err != nil || blocked {
					t.Fatalf("paid account remains blocked: %v %v", blocked, err)
				}
				if status := getNodes(); status != http.StatusOK {
					t.Fatalf("paid activation did not restore node access: %d", status)
				}
				q.RemainingIncludedQuota -= 3 << 30
				if err := st.UpsertAccountQuotaState(ctx, q); err != nil {
					t.Fatal(err)
				}
				for _, replay := range []bool{false, true, false} {
					if err := h.syncSubscriptionEntitlements(ctx, sub, replay); err != nil {
						t.Fatal(err)
					}
				}
				q, _ = st.GetAccountQuotaState(ctx, u.ID)
				if q.RemainingIncludedQuota != 17<<30 {
					t.Fatalf("webhook replay refilled consumed quota: %d", q.RemainingIncludedQuota)
				}
			})
		}
	}
}
