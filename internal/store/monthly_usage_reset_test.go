package store

import (
	"context"
	"testing"
	"time"
)

func TestMonthlyUsageResetPreservesHistoryAndExpires(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore().(*memoryStore)
	u := &User{Name: "reset", Email: "reset@example.invalid", Active: true}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if now.Day() == 1 {
		now = now.Add(48 * time.Hour)
	}
	_ = s.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: u.ID, IncludedQuotaBytes: 100})
	_ = s.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 10, CurrentBalance: -5, Arrears: true, SuspendState: "suspended", ProxyAccessState: "paused"})
	_ = s.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{AccountUUID: u.ID, NodeID: "n", BucketStart: now.Add(-time.Hour), TotalBytes: 90})
	q, err := s.ResetMonthlyUsage(ctx, u.ID, "actor", u.Email, "approved correction", now)
	if err != nil {
		t.Fatal(err)
	}
	if q.RemainingIncludedQuota != 100 || q.CurrentBalance != -5 || !q.Arrears || q.SuspendState != "suspended" || q.ProxyAccessState != "paused" {
		t.Fatalf("state changed: %+v", q)
	}
	if q.PeriodEnd.Day() != 1 || q.PeriodEnd.Hour() != 0 || !q.PeriodStart.Equal(now.Truncate(time.Minute).Add(time.Minute)) {
		t.Fatal("incorrect UTC bounds")
	}
	if len(s.trafficMinuteBuckets) != 1 || len(s.auditLogs) == 0 {
		t.Fatal("history lost")
	}
	end := *q.PeriodEnd
	_ = s.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{AccountUUID: u.ID, NodeID: "n", BucketStart: end.Add(time.Minute), TotalBytes: 7})
	s.mu.Lock()
	s.expireMonthlyUsageResetLocked(u.ID, end.Add(time.Hour))
	s.mu.Unlock()
	q, _ = s.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 93 || !q.PeriodStart.Equal(end) {
		t.Fatalf("month rollover: %+v", q)
	}
	if _, err = s.ResetMonthlyUsage(ctx, u.ID, "actor", "wrong@example.invalid", "test", now); err == nil {
		t.Fatal("wrong email accepted")
	}
}

func TestUpgradeAfterMonthlyResetPreservesResetBoundary(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore().(*memoryStore)
	u := &User{Name: "upgrade-reset", Email: "upgrade-reset@example.invalid"}
	_ = s.CreateUser(ctx, u)
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	cutoff := start.Add(48 * time.Hour)
	_ = s.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: u.ID, IncludedQuotaBytes: 100, PricingRuleVersion: "plan:PLUS"})
	_ = s.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 93, PeriodStart: &cutoff, PeriodEnd: &end})
	if err := s.UpgradeSubscriptionQuota(ctx, u.ID, 100, 150, start, end); err != nil {
		t.Fatal(err)
	}
	q, _ := s.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 143 || !q.PeriodStart.Equal(cutoff) {
		t.Fatalf("upgrade erased reset boundary: %+v", q)
	}
}
