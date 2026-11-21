package store

import (
	"context"
	"testing"
	"time"

	"account/internal/routingpolicy"
)

func TestDualBucketSubscriptionActivationUpgradeAndMonthlyReset(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	user := &User{
		Name:          "Dual Bucket User",
		Email:         "dual@example.com",
		PasswordHash:  "hash",
		EmailVerified: true,
		Active:        true,
		Role:          RoleUser,
		Groups:        []string{MonthlyFreeQuotaLimitGroup},
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	const (
		freeVPS = int64(5 << 30)
		plusVPS = int64(30 << 30)
		proVPS  = int64(50 << 30)
		proHS   = int64(30 << 30)
	)
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{
		AccountUUID:        user.ID,
		PackageName:        "free",
		IncludedQuotaBytes: freeVPS,
		PricingRuleVersion: "plan:FREE",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{
		AccountUUID:            user.ID,
		RemainingIncludedQuota: 2 << 30,
		PeriodStart:            &start,
		PeriodEnd:              &end,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}

	// Activate FREE -> PRO-MONTHLY-V2 atomically grants both buckets.
	if err := st.ActivateSubscriptionDualQuota(ctx, user.ID, freeVPS, proVPS, 0, proHS, start, end); err != nil {
		t.Fatalf("ActivateSubscriptionDualQuota: %v", err)
	}
	p, err := st.GetAccountBillingProfile(ctx, user.ID)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	q, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if p.IncludedQuotaBytes != proVPS || p.HighSpeedQuotaBytes != proHS {
		t.Fatalf("profile buckets = %d/%d, want %d/%d", p.IncludedQuotaBytes, p.HighSpeedQuotaBytes, proVPS, proHS)
	}
	if q.RemainingIncludedQuota != proVPS || q.RemainingHighSpeedQuota != proHS {
		t.Fatalf("quota buckets = %d/%d, want %d/%d", q.RemainingIncludedQuota, q.RemainingHighSpeedQuota, proVPS, proHS)
	}

	// Reset to Plus (30 GiB VPS, 0 high-speed), consume 10 GiB VPS, then upgrade to Pro (50 GiB VPS, 30 GiB high-speed).
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{
		AccountUUID:        user.ID,
		PackageName:        "plus",
		IncludedQuotaBytes: plusVPS,
		PricingRuleVersion: "plan:PLUS",
	}); err != nil {
		t.Fatalf("set plus profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{
		AccountUUID:            user.ID,
		RemainingIncludedQuota: 20 << 30,
		PeriodStart:            &start,
		PeriodEnd:              &end,
	}); err != nil {
		t.Fatalf("set plus quota: %v", err)
	}
	if err := st.UpgradeSubscriptionDualQuota(ctx, user.ID, plusVPS, proVPS, 0, proHS, start, end); err != nil {
		t.Fatalf("UpgradeSubscriptionDualQuota: %v", err)
	}
	q, err = st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("get upgraded quota: %v", err)
	}
	if q.RemainingIncludedQuota != 40<<30 || q.RemainingHighSpeedQuota != proHS {
		t.Fatalf("upgraded remaining = %d/%d, want %d/%d (10GiB VPS usage preserved)", q.RemainingIncludedQuota, q.RemainingHighSpeedQuota, 40<<30, proHS)
	}

	// Simulate consumption on both buckets then verify monthly reset restores both.
	q.RemainingIncludedQuota = 12 << 30
	q.RemainingHighSpeedQuota = 3 << 30
	if err := st.UpsertAccountQuotaState(ctx, q); err != nil {
		t.Fatalf("consume quota: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{
		AccountUUID:         user.ID,
		PackageName:         "pro",
		IncludedQuotaBytes:  proVPS,
		HighSpeedQuotaBytes: proHS,
		PricingRuleVersion:  "plan:PRO-MONTHLY-V2",
	}); err != nil {
		t.Fatalf("update pro profile: %v", err)
	}
	resetAt := time.Date(now.Year(), now.Month(), 15, 10, 0, 0, 0, time.UTC)
	resetState, err := st.ResetMonthlyUsage(ctx, user.ID, "admin-1", user.Email, "goodwill reset", resetAt)
	if err != nil {
		t.Fatalf("ResetMonthlyUsage: %v", err)
	}
	if resetState.RemainingIncludedQuota != proVPS || resetState.RemainingHighSpeedQuota != proHS {
		t.Fatalf("after monthly reset = %d/%d, want %d/%d", resetState.RemainingIncludedQuota, resetState.RemainingHighSpeedQuota, proVPS, proHS)
	}
}

func TestDualBucketAdminPlanGroupPreviewApplyAndDowngrade(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	user := &User{
		Name:          "Admin Plan Switch",
		Email:         "switch@example.com",
		PasswordHash:  "hash",
		EmailVerified: true,
		Active:        true,
		Role:          RoleUser,
		Groups:        []string{MonthlyPlusQuotaLimitGroup},
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{
		AccountUUID:         user.ID,
		PackageName:         "pro",
		IncludedQuotaBytes:  50 << 30,
		HighSpeedQuotaBytes: 30 << 30,
		PricingRuleVersion:  "plan:PRO-MONTHLY-V2",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{
		AccountUUID:             user.ID,
		RemainingIncludedQuota:  40 << 30, // 10 GiB VPS used
		RemainingHighSpeedQuota: 18 << 30, // 12 GiB HS used
		PeriodStart:             &start,
		PeriodEnd:               &end,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	if err := st.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{
		BucketStart: start.Add(time.Hour),
		NodeID:      "prod:jp-xconnect-pro-01.svc.plus",
		AccountUUID: user.ID,
		RoutingPool: routingpolicy.RoutingPoolProEC2,
		TotalBytes:  12 << 30,
	}); err != nil {
		t.Fatalf("seed pro bucket: %v", err)
	}
	if err := st.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{
		BucketStart: start.Add(2 * time.Hour),
		NodeID:      "prod:jp-xconnect-01.svc.plus",
		AccountUUID: user.ID,
		RoutingPool: routingpolicy.RoutingPoolMediaPriority,
		TotalBytes:  10 << 30,
	}); err != nil {
		t.Fatalf("seed media bucket: %v", err)
	}

	buckets, err := st.ListTrafficMinuteBucketsByAccount(ctx, user.ID, time.Time{}, time.Time{})
	if err != nil || len(buckets) != 2 {
		t.Fatalf("buckets = %d err=%v", len(buckets), err)
	}
	if buckets[0].RoutingPool != routingpolicy.RoutingPoolProEC2 {
		t.Fatalf("bucket[0].RoutingPool = %q, want Pro-EC2-Pool", buckets[0].RoutingPool)
	}

	// Downgrade Pro -> Plus: HighSpeedQuotaBytes and RemainingHighSpeedQuota must immediately become 0,
	// while VPS preserves its 10 GiB usage (30 - 10 = 20 GiB).
	change := AdminPlanGroupChange{
		UserID:              user.ID,
		PlanID:              "PLUS",
		SetEntitlement:      true,
		ExpectedGroups:      user.Groups,
		Groups:              []string{MonthlyPlusQuotaLimitGroup},
		PackageName:         "plus",
		IncludedQuotaBytes:  30 << 30,
		HighSpeedQuotaBytes: 0,
		RegionMultiplier:    1,
		LineMultiplier:      1,
		PeakMultiplier:      1,
		OffPeakMultiplier:   1,
		PricingRuleVersion:  "plan:PLUS",
	}
	preview, err := st.CreateAdminPlanGroupPreview(ctx, AdminPlanGroupPreview{
		ActorUUID: "root-1",
		Reason:    "downgrade to plus",
		RequestID: "req-downgrade-1",
		TokenHash: "tok-downgrade-1",
		ExpiresAt: now.Add(time.Minute),
		Changes:   []AdminPlanGroupChange{change},
	})
	if err != nil {
		t.Fatalf("preview downgrade: %v", err)
	}
	if preview[0].RemainingAfter != 20<<30 || preview[0].HighSpeedRemainingAfter != 0 {
		t.Fatalf("preview remaining = %d/%d, want %d/0", preview[0].RemainingAfter, preview[0].HighSpeedRemainingAfter, 20<<30)
	}
	if _, _, err := st.ApplyAdminPlanGroupBatch(ctx, AdminPlanGroupBatch{
		ActorUUID:        "root-1",
		Reason:           "downgrade to plus",
		RequestID:        "req-downgrade-1",
		PreviewTokenHash: "tok-downgrade-1",
	}); err != nil {
		t.Fatalf("apply downgrade: %v", err)
	}

	p, _ := st.GetAccountBillingProfile(ctx, user.ID)
	q, _ := st.GetAccountQuotaState(ctx, user.ID)
	if p.HighSpeedQuotaBytes != 0 || q.RemainingHighSpeedQuota != 0 {
		t.Fatalf("downgraded high-speed not zeroed: profile=%d remaining=%d", p.HighSpeedQuotaBytes, q.RemainingHighSpeedQuota)
	}
	if p.IncludedQuotaBytes != 30<<30 || q.RemainingIncludedQuota != 20<<30 {
		t.Fatalf("downgraded VPS = %d/%d, want %d/%d", p.IncludedQuotaBytes, q.RemainingIncludedQuota, 30<<30, 20<<30)
	}
}
