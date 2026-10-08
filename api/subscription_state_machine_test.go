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

func TestExpiredPlusPreservesOverageAndPaidRestorationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Overage User", "overage@example.com", "proxy-overage")
	user.Groups = []string{store.MonthlyPlusQuotaLimitGroup, "operator:kept"}
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	plus := &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30, Active: true}
	free := &store.BillingPlan{PlanID: store.BillingPlanFree, PackageName: "free", IncludedQuotaBytes: 5 << 30, Active: true}
	for _, plan := range []*store.BillingPlan{plus, free} {
		if err := st.UpsertBillingPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	h := &handler{store: st}
	if err := h.applyPlanEntitlements(ctx, user.ID, plus); err != nil {
		t.Fatal(err)
	}
	start, end := naturalMonthPeriod(time.Now().UTC())
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: user.ID, RemainingIncludedQuota: 1 << 30, PeriodStart: &start, PeriodEnd: &end,
	}); err != nil {
		t.Fatal(err)
	}
	expiredAt := time.Now().UTC().Add(-time.Hour)
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: plus.PlanID,
		ExternalID: "sub-overage", Status: "canceled", Meta: map[string]any{"expiresAt": expiredAt.Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if err := h.reconcileSubscriptionAccessAt(ctx, user, now); err != nil {
		t.Fatal(err)
	}
	downgraded, _ := st.GetAccountQuotaState(ctx, user.ID)
	if want := int64(-14 * (1 << 30)); downgraded.RemainingIncludedQuota != want {
		t.Fatalf("Free downgrade must preserve 19GiB already used and expose overage, got remaining=%d want=%d", downgraded.RemainingIncludedQuota, want)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	repeated, _ := st.GetAccountQuotaState(ctx, user.ID)
	if repeated.RemainingIncludedQuota != downgraded.RemainingIncludedQuota || !repeated.EffectiveAt.Equal(downgraded.EffectiveAt) {
		t.Fatalf("repeated downgrade reconciliation changed quota: first=%+v repeated=%+v", downgraded, repeated)
	}

	periodStart := time.Now().UTC().Add(-24 * time.Hour)
	periodEnd := time.Now().UTC().AddDate(0, 1, 0)
	restored := &stripeSubscription{
		ID: "sub-overage", Status: "active", Metadata: map[string]string{"user_id": user.ID, "plan_id": plus.PlanID},
		CurrentPeriodStart: periodStart.Unix(), CurrentPeriodEnd: periodEnd.Unix(),
	}
	for range 2 {
		if err := h.syncSubscriptionEntitlements(ctx, restored, false); err != nil {
			t.Fatal(err)
		}
	}
	restoredQuota, _ := st.GetAccountQuotaState(ctx, user.ID)
	restoredProfile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	if restoredQuota.RemainingIncludedQuota != 1<<30 || restoredProfile.IncludedQuotaBytes != 20<<30 {
		t.Fatalf("paid restoration must restore Plus while retaining 19GiB usage exactly once: quota=%+v profile=%+v", restoredQuota, restoredProfile)
	}
	updatedUser, _ := st.GetUserByID(ctx, user.ID)
	if store.MonthlyQuotaGroup(updatedUser) != store.MonthlyPlusQuotaLimitGroup {
		t.Fatalf("paid restoration did not restore Plus access group: %v", updatedUser.Groups)
	}
}

func TestSubscriptionSweepProcessesBoundedPagesUntilAllUsersAreReconciled(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	now := time.Now().UTC()
	for i := range 3 {
		user := seedActiveUser(t, st, "Sweep User "+strconv.Itoa(i), "sweep"+strconv.Itoa(i)+"@example.com", "proxy-sweep-"+strconv.Itoa(i))
		if err := st.UpsertSubscription(ctx, &store.Subscription{
			UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-M", ExternalID: "sub-sweep-" + strconv.Itoa(i),
			Status: "canceled", Meta: map[string]any{"expiresAt": now.Add(-time.Hour).Format(time.RFC3339)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	ordered, err := st.ListUsersPage(ctx, "", store.MaxUserListPageSize)
	if err != nil {
		t.Fatal(err)
	}
	cursor, processed, err := ReconcileSubscriptionAccessBatch(ctx, st, now, "", 2)
	if err != nil || processed != 2 || cursor != ordered[1].ID {
		t.Fatalf("first sweep batch not bounded/cursored: cursor=%q processed=%d err=%v", cursor, processed, err)
	}
	for _, user := range ordered[:2] {
		profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
		if profile == nil || profile.PricingRuleVersion != "plan:FREE" {
			t.Fatalf("first page account %s was not reconciled: %+v", user.ID, profile)
		}
	}
	nextCursor, processed, err := ReconcileSubscriptionAccessBatch(ctx, st, now, cursor, 2)
	if err != nil || processed != 1 || nextCursor != "" {
		t.Fatalf("second sweep batch did not finish and wrap: cursor=%q processed=%d err=%v", nextCursor, processed, err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, ordered[2].ID)
	if profile == nil || profile.PricingRuleVersion != "plan:FREE" {
		t.Fatalf("last account was not reconciled by sweep: %+v", profile)
	}
}

func TestMissingStripePeriodGetsOnlyBoundedRetryWindow(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Missing Period", "missing-period@example.com", "proxy-missing-period")
	h := &handler{store: st}
	paid := &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30}
	if err := h.applyPlanEntitlements(ctx, user.ID, paid); err != nil {
		t.Fatal(err)
	}
	oldStart := time.Now().UTC().Add(-48 * time.Hour)
	oldEnd := time.Now().UTC().Add(-24 * time.Hour)
	if err := h.resetQuotaForPlan(ctx, user.ID, paid, oldStart, oldEnd); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: paid.PlanID,
		ExternalID: "sub-missing-period", Status: "active", Meta: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	subscriptions, _ := st.ListSubscriptionsByUser(ctx, user.ID)
	observedAt := subscriptions[0].CreatedAt
	// Repeated Stripe observations must not restart the missing-period clock.
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: paid.PlanID,
		ExternalID: "sub-missing-period", Status: "active", Meta: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, observedAt.Add(missingStripePeriodRetryWindow-time.Second)); err != nil {
		t.Fatal(err)
	}
	profile, _ := st.GetAccountBillingProfile(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:PRO-M" {
		t.Fatalf("missing period was treated as expired before retry window: %+v", profile)
	}
	if err := h.reconcileSubscriptionAccessAt(ctx, user, observedAt.Add(missingStripePeriodRetryWindow)); err != nil {
		t.Fatal(err)
	}
	profile, _ = st.GetAccountBillingProfile(ctx, user.ID)
	if profile.PricingRuleVersion != "plan:FREE" {
		t.Fatalf("missing Stripe period granted indefinite paid access: %+v", profile)
	}
}

func TestAgentClientListPreservesAccessAfterOverQuotaDowngrade(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := seedActiveUser(t, st, "Immediate Cutoff", "immediate-cutoff@example.com", "proxy-immediate-cutoff")
	user.Groups = []string{store.MonthlyPlusQuotaLimitGroup}
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	paid := &store.BillingPlan{PlanID: "PRO-M", PackageName: "pro", IncludedQuotaBytes: 20 << 30}
	if err := h.applyPlanEntitlements(ctx, user.ID, paid); err != nil {
		t.Fatal(err)
	}
	start, end := naturalMonthPeriod(time.Now().UTC())
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 1 << 30, PeriodStart: &start, PeriodEnd: &end}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: paid.PlanID, ExternalID: "sub-immediate-cutoff",
		Status: "canceled", Meta: map[string]any{"expiresAt": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	clients, _, err := h.authorizedAgentClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, client := range clients {
		if client.ID == user.ProxyUUID {
			found = true
		}
	}
	if !found {
		t.Fatal("newly downgraded over-quota user must remain authorized")
	}
	quota, _ := st.GetAccountQuotaState(ctx, user.ID)
	if quota.RemainingIncludedQuota != -14*(1<<30) {
		t.Fatalf("agent reconciliation did not preserve consumed quota: %+v", quota)
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
