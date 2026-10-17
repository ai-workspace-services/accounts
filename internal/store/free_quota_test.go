package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestFreeNaturalMonthResetIsUTCAndDoesNotGrantTwice(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	u := &User{Name: "free-calendar", Email: "calendar@example.invalid", Active: true, Groups: []string{MonthlyFreeQuotaLimitGroup}}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	old := start.AddDate(0, -1, 0)
	if err := s.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: u.ID, IncludedQuotaBytes: 5 << 30, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 0, PeriodStart: &old, PeriodEnd: &start, ProxyAccessState: "paused", SuspendState: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{AccountUUID: u.ID, NodeID: "node", BucketStart: start.Add(time.Minute), TotalBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ResetFreeQuotaPeriod(ctx, u.ID, start, end, start.Add(time.Hour)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	q, _ := s.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 4<<30 || !q.PeriodStart.Equal(start) || !q.PeriodEnd.Equal(end) || q.ProxyAccessState != "paused" {
		t.Fatalf("incorrect month rollover: %+v", q)
	}
	q.RemainingIncludedQuota = 1 << 30
	_ = s.UpsertAccountQuotaState(ctx, q)
	_ = s.ResetFreeQuotaPeriod(ctx, u.ID, start, end, start.Add(2*time.Hour))
	q, _ = s.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 1<<30 {
		t.Fatal("repeat reset erased current usage")
	}
}
