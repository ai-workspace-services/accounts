package api

import (
	"context"
	"testing"
	"time"

	"account/internal/store"
)

func TestReconcileAnnualPlanQuotasResetsOnlyExpiredAnnualGrant(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "annual", Email: "annual@example.test", Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: "PRO-YEARLY", Kind: "subscription", PriceUnit: "year", IncludedQuotaBytes: 20_000, Active: true,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-YEARLY", ExternalID: "sub_year", Status: "active",
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Hour)
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 1, PeriodEnd: &expired}); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	count, err := ReconcileAnnualPlanQuotas(ctx, st, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if count != 1 {
		t.Fatalf("reset count = %d, want 1", count)
	}
	quota, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("load quota: %v", err)
	}
	if quota.RemainingIncludedQuota != 20_000 {
		t.Fatalf("quota = %d, want 20000", quota.RemainingIncludedQuota)
	}
	if quota.PeriodEnd == nil || !quota.PeriodEnd.Equal(time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected period end: %v", quota.PeriodEnd)
	}

	count, err = ReconcileAnnualPlanQuotas(ctx, st, now)
	if err != nil || count != 0 {
		t.Fatalf("second run = (%d, %v), want (0, nil)", count, err)
	}
}

func TestSubscriptionQuotaPeriodUsesMonthlyBoundsForAnnualPlan(t *testing.T) {
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	start, end := subscriptionQuotaPeriod(&store.BillingPlan{PriceUnit: "year"}, &stripeSubscription{CurrentPeriodStart: now.AddDate(0, 0, -10).Unix(), CurrentPeriodEnd: now.AddDate(1, 0, 0).Unix()}, now)
	if !start.Equal(time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("annual period = %s - %s", start, end)
	}
}
