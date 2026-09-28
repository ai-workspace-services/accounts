package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"account/internal/store"
)

func TestPaidSubscriptionAndManualValidityOutrankExpiredSnapshot(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Priority User", "priority@example.com", "proxy-priority")
	user.Groups = []string{store.MonthlyPlusQuotaLimitGroup, "operator:kept"}
	manualEnd := time.Now().UTC().Add(-30 * 24 * time.Hour)
	user.SubscriptionValidUntil = &manualEnd
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	plan := &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30, Active: true}
	if err := h.applyPlanEntitlements(ctx, user.ID, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(time.Now().UTC().Add(-24*time.Hour).Unix(), 0).UTC()
	end := time.Now().UTC().Add(time.Hour)
	if err := h.resetQuotaForPlan(ctx, user.ID, plan, start, end); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-M", ExternalID: "sub-priority", Status: "canceling",
		Meta: map[string]any{"expiresAt": end.Format(time.RFC3339), "cancelAtPeriodEnd": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.reconcileSubscriptionAccessAt(ctx, user, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	quota, _ := st.GetAccountQuotaState(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:PRO-M" || quota.RemainingIncludedQuota != 20<<30 {
		t.Fatalf("scheduled cancellation or expired manual date overrode live paid entitlement: profile=%+v quota=%+v", profile, quota)
	}

	// Once Stripe access has ended, a current local grant remains in force and
	// the persisted snapshot is not used as independent entitlement evidence.
	manualEnd = time.Now().UTC().Add(30 * 24 * time.Hour)
	user.SubscriptionValidUntil = &manualEnd
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, time.Now().UTC().Add(20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	profile, _ = st.GetAccountBillingProfile(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:PRO-M" {
		t.Fatalf("current manual validity was ignored after Stripe grace: %+v", profile)
	}
}

func TestCanceledSubscriptionDowngradesAtPeriodEndAndIdempotently(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Grace User", "grace@example.com", "proxy-grace")
	user.Groups = []string{store.MonthlyPlusQuotaLimitGroup, "operator:kept"}
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	paid := &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30}
	if err := h.applyPlanEntitlements(ctx, user.ID, paid); err != nil {
		t.Fatal(err)
	}
	endedAt := time.Now().UTC().Add(time.Hour)
	start := endedAt.Add(-30 * 24 * time.Hour)
	if err := h.resetQuotaForPlan(ctx, user.ID, paid, start, endedAt); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-M", ExternalID: "sub-grace", Status: "cancelled",
		Meta: map[string]any{"expiresAt": endedAt.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}

	beforeEnd := endedAt.Add(-time.Second)
	if err := h.reconcileSubscriptionAccessAt(ctx, user, beforeEnd); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:PRO-M" {
		t.Fatalf("subscription downgraded before period end: %+v", profile)
	}

	afterEnd := endedAt.Add(time.Second)
	if err := h.reconcileSubscriptionAccessAt(ctx, user, afterEnd); err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetAccountQuotaState(ctx, user.ID)
	if first.RemainingIncludedQuota != defaultFreeQuotaBytes {
		t.Fatalf("expected Free 5GiB after period end, got %+v", first)
	}
	operatorGroupKept := false
	for _, group := range user.Groups {
		operatorGroupKept = operatorGroupKept || group == "operator:kept"
	}
	if !operatorGroupKept || store.MonthlyQuotaGroup(user) != store.MonthlyFreeQuotaLimitGroup {
		t.Fatalf("user group attributes were not preserved: %v", user.Groups)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, afterEnd.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	second, _ := st.GetAccountQuotaState(ctx, user.ID)
	if !second.EffectiveAt.Equal(first.EffectiveAt) || second.RemainingIncludedQuota != first.RemainingIncludedQuota {
		t.Fatalf("repeated read reconciliation re-armed quota: first=%+v second=%+v", first, second)
	}
}

func TestPaymentFailureGraceAndQuotaPeriodResetAreIdempotent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{store: st}
	plan := &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 100}
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if err := h.resetQuotaForPlanOnce(ctx, "user-1", plan, start, end); err != nil {
		t.Fatal(err)
	}
	first, _ := st.GetAccountQuotaState(ctx, "user-1")
	if err := h.resetQuotaForPlanOnce(ctx, "user-1", plan, start, end); err != nil {
		t.Fatal(err)
	}
	second, _ := st.GetAccountQuotaState(ctx, "user-1")
	if !second.EffectiveAt.Equal(first.EffectiveAt) {
		t.Fatalf("same period reset quota twice: first=%v second=%v", first.EffectiveAt, second.EffectiveAt)
	}
	failedAt := time.Now().UTC().Add(-time.Hour)
	second.RemainingIncludedQuota = 42
	second.Arrears = true
	second.ArrearsSince = &failedAt
	second.ThrottleState = "throttled"
	if err := st.UpsertAccountQuotaState(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := h.resetQuotaForPlanOnce(ctx, "user-1", plan, start, end); err != nil {
		t.Fatal(err)
	}
	settled, _ := st.GetAccountQuotaState(ctx, "user-1")
	if settled.RemainingIncludedQuota != 42 || settled.Arrears || settled.ArrearsSince != nil || settled.ThrottleState != "normal" {
		t.Fatalf("same-period invoice payment must clear dunning without resetting quota: %+v", settled)
	}

	user := seedActiveUser(t, st, "Arrears User", "arrears@example.com", "proxy-arrears")
	user.Groups = []string{store.MonthlyPlusQuotaLimitGroup}
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Add(-subscriptionGracePeriod + time.Minute)
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 100, Arrears: true, ArrearsSince: &since}); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	if profile != nil && profile.PricingRuleVersion == "plan:FREE" {
		t.Fatalf("payment failure downgraded before the explicit 14-day grace elapsed: %+v", profile)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	profile, _ = st.GetAccountBillingProfile(ctx, user.ID)
	if profile == nil || profile.PricingRuleVersion != "plan:FREE" {
		t.Fatalf("payment failure did not downgrade after the explicit 14-day grace: %+v", profile)
	}
	if err := h.restoreQuotaGroupForPlan(ctx, user.ID, &store.BillingPlan{IncludedQuotaBytes: 20 << 30}); err != nil {
		t.Fatal(err)
	}
	updatedUser, _ := st.GetUserByID(ctx, user.ID)
	if store.MonthlyQuotaGroup(updatedUser) != store.MonthlyPlusQuotaLimitGroup {
		t.Fatalf("paid recovery did not restore the plan quota group: %v", updatedUser.Groups)
	}
}

func TestSubscriptionUpgradeAppliesImmediatelyWithoutResettingUsageTwice(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Upgrade User", "upgrade@example.com", "proxy-upgrade")
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "PRO-L", PackageName: "pro-large", IncludedQuotaBytes: 200, Active: true}); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	if err := h.applyPlanEntitlements(ctx, user.ID, &store.BillingPlan{PlanID: "PRO-S", PackageName: "pro-small", IncludedQuotaBytes: 100}); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(time.Now().UTC().Add(-24*time.Hour).Unix(), 0).UTC()
	end := start.AddDate(0, 1, 0)
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 40, PeriodStart: &start, PeriodEnd: &end}); err != nil {
		t.Fatal(err)
	}
	updated := &stripeSubscription{
		ID: "sub-upgrade", Status: "active", Metadata: map[string]string{"user_id": user.ID, "plan_id": "PRO-L"},
		CurrentPeriodStart: start.Unix(), CurrentPeriodEnd: end.Unix(),
	}
	for range 2 {
		if err := h.syncSubscriptionEntitlements(ctx, updated, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.applyPlanUpgradeQuota(ctx, user.ID, 100, 200, start, end); err != nil {
		t.Fatal(err)
	}
	quota, _ := st.GetAccountQuotaState(ctx, user.ID)
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	if quota.RemainingIncludedQuota != 140 || profile.IncludedQuotaBytes != 200 {
		t.Fatalf("upgrade did not apply immediately and preserve prior usage exactly once: quota=%+v profile=%+v", quota, profile)
	}
}

func TestManualValidityExpiresAtNextUTCDateWithoutGrace(t *testing.T) {
	validUntil := time.Date(2026, time.September, 27, 0, 0, 0, 0, time.UTC)
	user := &store.User{SubscriptionValidUntil: &validUntil}
	expiresAt := time.Date(2026, time.September, 28, 0, 0, 0, 0, time.UTC)
	if subscriptionValidityExpired(user, expiresAt.Add(-time.Nanosecond)) {
		t.Fatal("manual validity expired before the inclusive end date completed")
	}
	if !subscriptionValidityExpired(user, expiresAt) {
		t.Fatal("manual validity did not expire at next-day UTC midnight")
	}
}

func TestScheduleSubscriptionCancellationUsesPeriodEndUpdate(t *testing.T) {
	var method, requestPath, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, requestPath = r.Method, r.URL.Path
		data, _ := url.ParseQuery(readTestBody(r))
		body = data.Get("cancel_at_period_end")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"sub_1"}`))
	}))
	defer server.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test"})
	client.httpClient = server.Client()
	// Stripe's endpoint is fixed to HTTPS; a custom transport redirects the
	// request to this local test server while preserving the request fields.
	client.httpClient.Transport = redirectStripeTestTransport{target: server.URL}
	if _, err := client.scheduleSubscriptionCancellation(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || requestPath != "/v1/subscriptions/sub_1" || body != "true" {
		t.Fatalf("cancellation was not scheduled at period end: method=%s path=%s cancel_at_period_end=%q", method, requestPath, body)
	}
}

func TestRefundReconciliationRequiresExplicitCompletedFullRefund(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Refund User", "refund@example.com", "proxy-refund")
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: store.BillingPlanFree, PackageName: "free", IncludedQuotaBytes: defaultFreeQuotaBytes, Active: true}); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st, stripe: newStripeClient(StripeConfig{SecretKey: "sk_test"})}
	if err := h.applyPlanEntitlements(ctx, user.ID, &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := h.resetQuotaForPlan(ctx, user.ID, &store.BillingPlan{PlanID: "PRO-M", IncludedQuotaBytes: 20 << 30}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/invoices/in_1":
			_, _ = w.Write([]byte(`{"id":"in_1","subscription":"sub_refund"}`))
		case "/v1/subscriptions/sub_refund":
			_, _ = w.Write([]byte(`{"id":"sub_refund","status":"active","customer":"cus_1","metadata":{"user_id":"` + user.ID + `","plan_id":"PRO-M"},"current_period_start":` + strconv.FormatInt(time.Now().UTC().Add(-time.Hour).Unix(), 10) + `,"current_period_end":` + strconv.FormatInt(time.Now().UTC().Add(time.Hour).Unix(), 10) + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h.stripe.httpClient = server.Client()
	h.stripe.httpClient.Transport = redirectStripeTestTransport{target: server.URL}
	postRefund := func(body string) error {
		return h.handleStripeEvent(ctx, stripeEvent{
			ID: "evt_refund", Type: "charge.refunded",
			Data: struct {
				Object json.RawMessage `json:"object"`
			}{Object: json.RawMessage(body)},
		})
	}
	if err := postRefund(`{"invoice":"in_1","refunded":false,"amount":1000,"amount_refunded":500}`); err != nil {
		t.Fatal(err)
	}
	if err := postRefund(`{"invoice":"in_1","refunded":true,"amount":1000,"amount_refunded":1000,"refunds":{"data":[{"status":"pending"}]}}`); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:PRO-M" {
		t.Fatalf("partial or pending refund revoked entitlements: %+v", profile)
	}
	if err := postRefund(`{"invoice":"in_1","refunded":true,"amount":1000,"amount_refunded":1000,"refunds":{"data":[{"status":"succeeded"}]}}`); err != nil {
		t.Fatal(err)
	}
	profile, _ = st.GetAccountBillingProfile(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:FREE" || profile.IncludedQuotaBytes != defaultFreeQuotaBytes {
		t.Fatalf("completed full refund did not revoke paid entitlement to Free 5GiB: %+v", profile)
	}
	subscriptions, _ := st.ListSubscriptionsByUser(ctx, user.ID)
	if len(subscriptions) != 1 || subscriptions[0].Status != "refunded" {
		t.Fatalf("refund must retain and mark the subscription record: %+v", subscriptions)
	}
}

type redirectStripeTestTransport struct{ target string }

func (t redirectStripeTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

func readTestBody(r *http.Request) string {
	data, _ := io.ReadAll(r.Body)
	return string(data)
}
