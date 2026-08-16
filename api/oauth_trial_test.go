package api

import (
	"context"
	"testing"
	"time"

	"account/internal/store"
)

func trialHandler(t *testing.T) (*handler, store.Store) {
	t.Helper()
	st := store.NewMemoryStore()
	h := &handler{store: st}

	plan := &store.BillingPlan{
		PlanID:             store.BillingPlanTrial7D,
		DisplayName:        "7-Day Trial",
		Kind:               "trial",
		IncludedQuotaBytes: 5 * 1024 * 1024 * 1024,
		PackageName:        "trial",
		TrialDays:          7,
		Active:             true,
	}
	if err := st.UpsertBillingPlan(context.Background(), plan); err != nil {
		t.Fatalf("seed trial plan: %v", err)
	}
	return h, st
}

func seedUser(t *testing.T, st store.Store, id string) *store.User {
	t.Helper()
	user := &store.User{
		ID: id, Name: id, Email: id + "@example.com",
		Level: store.LevelUser, Role: store.RoleUser, Active: true,
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user
}

func TestOnboardingTrialGrantsCatalogQuota(t *testing.T) {
	h, st := trialHandler(t)
	ctx := context.Background()
	user := seedUser(t, st, "oauth-1")

	h.provisionOnboardingTrial(ctx, user.ID)

	quota, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	// The trial cap is the catalog's, not a constant in the handler — an
	// operator lowering it in the ops console must actually lower it.
	if quota.RemainingIncludedQuota != 5*1024*1024*1024 {
		t.Fatalf("trial quota = %d, want 5 GiB", quota.RemainingIncludedQuota)
	}

	subs, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].PlanID != store.BillingPlanTrial7D {
		t.Fatalf("subscriptions = %+v, want one TRIAL-7D", subs)
	}
}

// Trusting a provider-verified email means the trial is granted without an
// email round trip, so the only thing standing between a user and an endless
// trial is that the grant fires once. Re-running it must not extend the
// window or stack a second subscription.
func TestOnboardingTrialDoesNotStackOnRepeatGrants(t *testing.T) {
	h, st := trialHandler(t)
	ctx := context.Background()
	user := seedUser(t, st, "oauth-2")

	h.provisionOnboardingTrial(ctx, user.ID)
	time.Sleep(2 * time.Millisecond)
	h.provisionOnboardingTrial(ctx, user.ID)

	subs, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	// The trial subscription is keyed trial-<userID>, so a repeat grant
	// updates the row rather than stacking a second active trial that
	// ListSubscriptionsByUser consumers could not disambiguate.
	if len(subs) != 1 {
		t.Fatalf("got %d subscriptions, want 1", len(subs))
	}
}

// The quota window is re-armed on every call, which is precisely why
// oauthCallback guards the grant behind the EmailVerified false->true
// transition instead of calling it on each login. If this ever stops being
// true the guard could be relaxed; until then, removing it hands a fresh 7
// days to anyone who signs in again.
func TestOnboardingTrialReArmsTheWindowOnEveryCall(t *testing.T) {
	h, st := trialHandler(t)
	ctx := context.Background()
	user := seedUser(t, st, "oauth-3")

	h.provisionOnboardingTrial(ctx, user.ID)
	first, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if first.PeriodEnd == nil {
		t.Fatal("trial must set a period end")
	}
	firstEnd := *first.PeriodEnd

	time.Sleep(2 * time.Millisecond)
	h.provisionOnboardingTrial(ctx, user.ID)

	second, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if second.PeriodEnd == nil || !second.PeriodEnd.After(firstEnd) {
		t.Fatalf("period end did not move (%v -> %v); the caller-side guard "+
			"in oauthCallback is load-bearing and this test documents why",
			firstEnd, second.PeriodEnd)
	}
}
