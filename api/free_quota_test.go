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
	u := &store.User{Name: "monthly-free", Email: "monthly-free@example.invalid", Active: true, Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, IncludedQuotaBytes: defaultFreeQuotaBytes, PricingRuleVersion: "plan:FREE"}); err != nil {
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
	if q.RemainingIncludedQuota != 5<<30 {
		t.Fatalf("new month grant: %d", q.RemainingIncludedQuota)
	}
	if exhausted, err := h.quotaExhaustedForUser(ctx, u); err != nil || exhausted {
		t.Fatalf("new month did not recover: %v %v", exhausted, err)
	}
}
