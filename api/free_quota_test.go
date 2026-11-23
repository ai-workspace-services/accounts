package api

import (
	"account/internal/store"
	"context"
	"testing"
	"time"
)

func TestFreeQuotaExhaustionAndCalendarRecovery(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{store: st}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: store.BillingPlanWhitelistFree, DisplayName: "Whitelist Free", Kind: "whitelist_group",
		PackageName: "whitelist", IncludedQuotaBytes: 5 << 30, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	u := &store.User{Name: "whitelist-free", Email: "whitelist-free@example.invalid", Active: true, Groups: []string{store.WhitelistFreeQuotaGroup}}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, PackageName: "whitelist", IncludedQuotaBytes: 5 << 30, PricingRuleVersion: "plan:" + store.BillingPlanWhitelistFree}); err != nil {
		t.Fatal(err)
	}
	for _, remaining := range []int64{1, 0, -1} {
		if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: remaining, PeriodStart: &start, PeriodEnd: &end}); err != nil {
			t.Fatal(err)
		}
		exhausted, err := h.quotaExhaustedForUser(ctx, u)
		if err != nil || exhausted != (remaining <= 0) {
			t.Fatalf("remaining %d: exhausted=%v err=%v", remaining, exhausted, err)
		}
	}
	if err := h.ensureFreeQuotaPeriod(ctx, u, end.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	q, _ := st.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 5<<30 || q.ProxyAccessState != "active" {
		t.Fatalf("new month grant: %+v", q)
	}
	if exhausted, err := h.quotaExhaustedForUser(ctx, u); err != nil || exhausted {
		t.Fatalf("new month did not recover: %v %v", exhausted, err)
	}
}
