package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

type referralCompletionFixture struct {
	h                 *handler
	router            *gin.Engine
	referrer, invitee *store.User
	invoice           string
	historyFails      bool
}

func newReferralCompletionFixture(t *testing.T, pending, latest bool) *referralCompletionFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemoryStore()
	f := &referralCompletionFixture{}
	f.referrer = seedActiveUser(t, st, "Inviter", "completion-owner@example.test", "completion-owner")
	f.invitee = seedActiveUser(t, st, "Friend", "completion-friend@example.test", "completion-friend")
	if _, err := st.EnsureReferralCode(ctx, f.referrer.ID, "COMPLETE123"); err != nil {
		t.Fatal(err)
	}
	paid := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := st.CreateReferralAttribution(ctx, "COMPLETE123", f.invitee.ID, paid.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "PRO-MONTHLY", StripePriceID: "price-completion", Kind: "subscription", PriceUnit: "month", PackageName: "xconnect-pro", IncludedQuotaBytes: 10000, Active: true}); err != nil {
		t.Fatal(err)
	}
	f.invoice = fmt.Sprintf(`{"id":"in-completion-first","customer":"cus-completion","subscription":"sub-completion","status":"paid","payment_intent":"pi-completion","amount_paid":12345,"currency":"usd","billing_reason":"subscription_create","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price-completion","period":{"start":%d,"end":%d}}]}}`, paid.Unix(), paid.Unix(), paid.AddDate(0, 1, 0).Unix())
	current := "in-completion-renewal"
	if latest {
		current = "in-completion-first"
	}
	pendingJSON := "null"
	if pending {
		pendingJSON = `{"expires_at":1900000000}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/subscriptions/sub-completion":
			fmt.Fprintf(w, `{"id":"sub-completion","status":"active","customer":"cus-completion","latest_invoice":%q,"pending_update":%s,"metadata":{"user_id":%q,"plan_id":"PRO-MONTHLY","product_slug":"xconnect"},"current_period_start":%d,"current_period_end":%d,"items":{"data":[{"price":{"id":"price-completion"}}]}}`, current, pendingJSON, f.invitee.ID, paid.AddDate(0, 1, 0).Unix(), paid.AddDate(0, 2, 0).Unix())
		case "/v1/invoices":
			if f.historyFails {
				http.Error(w, "temporary history failure", 503)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"in-completion-first","subscription":"sub-completion","status":"paid","amount_paid":12345}],"has_more":false}`)
		case "/v1/invoices/in-completion-renewal":
			fmt.Fprint(w, `{"id":"in-completion-renewal","status":"paid","subscription":"sub-completion"}`)
		case "/v1/invoices/in-completion-first":
			fmt.Fprint(w, f.invoice)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newStripeClient(StripeConfig{SecretKey: "sk_test_fixture", WebhookSecret: testStripeWebhookSecret})
	client.apiBaseURL = server.URL + "/v1"
	client.httpClient = server.Client()
	if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: f.invitee.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub-completion", PlanID: "PRO-MONTHLY", Status: "active", Meta: map[string]any{"customer_id": "cus-completion"}}); err != nil {
		t.Fatal(err)
	}
	f.h = &handler{store: st, stripe: client}
	f.router = gin.New()
	f.router.POST("/webhook", f.h.stripeWebhook)
	return f
}

func (f *referralCompletionFixture) send(t *testing.T, eventID string) *httptest.ResponseRecorder {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"id":%q,"type":"invoice.paid","livemode":false,"data":{"object":%s}}`, eventID, f.invoice))
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
	req.Header.Set("Stripe-Signature", signStripePayload(t, payload))
	result := httptest.NewRecorder()
	f.router.ServeHTTP(result, req)
	return result
}

func TestReferralWebhookHistoricalAndPendingInvoicesAwardOnce(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pending, latest bool
	}{{"delayed-first-after-renewal", false, false}, {"pending-update", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReferralCompletionFixture(t, tc.pending, tc.latest)
			ctx := context.Background()
			start, end := time.Now().UTC(), time.Now().UTC().Add(time.Hour)
			if err := f.h.store.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: f.invitee.ID, RemainingIncludedQuota: 4321, PeriodStart: &start, PeriodEnd: &end}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"evt-completion", "evt-completion", "evt-completion-other"} {
				if response := f.send(t, id); response.Code != 200 {
					t.Fatalf("status %d: %s", response.Code, response.Body.String())
				}
			}
			rewards, err := f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10)
			if err != nil || len(rewards) != 1 || rewards[0].RewardMinor != 1234 || rewards[0].AnnualDays != 0 {
				t.Fatalf("rewards=%+v err=%v", rewards, err)
			}
			quota, err := f.h.store.GetAccountQuotaState(ctx, f.invitee.ID)
			if err != nil || quota.RemainingIncludedQuota != 4321 {
				t.Fatalf("historical invoice changed current quota: %+v %v", quota, err)
			}
			invoices, _ := f.h.store.ListFinanceInvoices(ctx, f.invitee.ID, "", 10)
			payments, _ := f.h.store.ListFinancePayments(ctx, f.invitee.ID, 10)
			if len(invoices) != 1 || len(payments) != 1 {
				t.Fatalf("duplicate financial facts: %d/%d", len(invoices), len(payments))
			}
		})
	}
}

func TestReferralWebhookRetryPreservesCoreEntitlements(t *testing.T) {
	f := newReferralCompletionFixture(t, false, true)
	f.historyFails = true
	if response := f.send(t, "evt-completion-retry"); response.Code != 502 {
		t.Fatalf("retry must fail: %d %s", response.Code, response.Body.String())
	}
	profile, err := f.h.store.GetAccountBillingProfile(context.Background(), f.invitee.ID)
	if err != nil || profile.PricingRuleVersion != "plan:PRO-MONTHLY" {
		t.Fatalf("paid entitlement not restored: %+v %v", profile, err)
	}
	f.historyFails = false
	if response := f.send(t, "evt-completion-retry"); response.Code != 200 {
		t.Fatalf("retry status: %d %s", response.Code, response.Body.String())
	}
	if response := f.send(t, "evt-completion-retry"); response.Code != 200 {
		t.Fatalf("duplicate status: %d", response.Code)
	}
	rewards, _ := f.h.store.ListReferralRewards(context.Background(), f.referrer.ID, 10)
	if len(rewards) != 1 {
		t.Fatalf("rewards=%+v", rewards)
	}
}

func TestReferralReconcilePreviewApplyAndReplay(t *testing.T) {
	f := newReferralCompletionFixture(t, false, false)
	ctx := context.Background()
	// Existing local subscription is required; preview must not upsert one itself.
	sub, err := f.h.stripe.fetchSubscription(ctx, "sub-completion")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.h.upsertStripeSubscription(ctx, sub, "cus-completion"); err != nil {
		t.Fatal(err)
	}
	req := ReferralReconcileRequest{AccountUUID: f.invitee.ID, InvoiceIDs: []string{"in-completion-first"}}
	items, err := f.h.reconcileReferralPaidInvoices(ctx, req)
	if err != nil || len(items) != 1 || items[0].Status != "candidate" {
		t.Fatalf("preview %+v %v", items, err)
	}
	invoices, _ := f.h.store.ListFinanceInvoices(ctx, f.invitee.ID, "", 10)
	rewards, _ := f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10)
	if len(invoices) != 0 || len(rewards) != 0 {
		t.Fatal("preview wrote local facts")
	}
	req.Apply = true
	for i := 0; i < 2; i++ {
		items, err = f.h.reconcileReferralPaidInvoices(ctx, req)
		if err != nil || items[0].Status != "reconciled" {
			t.Fatalf("apply %+v %v", items, err)
		}
	}
	rewards, _ = f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10)
	if len(rewards) != 1 {
		t.Fatalf("apply/replay rewards=%+v", rewards)
	}
	req.AccountUUID = f.referrer.ID
	if _, err = f.h.reconcileReferralPaidInvoices(ctx, req); err == nil {
		t.Fatal("cross-account invoice accepted")
	}
	req.AccountUUID = f.invitee.ID
	req.InvoiceIDs = make([]string, 51)
	if _, err = f.h.reconcileReferralPaidInvoices(ctx, req); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}
