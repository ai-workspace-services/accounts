package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

const gib = int64(1024 * 1024 * 1024)

func TestAdminPlanGroupBatchPreservesUsageAndQuotaAcrossPlanToggles(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{ID: "plan-user", Name: "plan user", Email: "plan@example.invalid", Role: RoleUser,
		Groups: []string{"segment:keep", MonthlyFreeQuotaLimitGroup}, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().AddDate(0, 0, -1).Truncate(time.Hour)
	end := start.AddDate(0, 1, 0)
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: user.ID, PackageName: "free", IncludedQuotaBytes: 5 * gib, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 1 * gib, PeriodStart: &start, PeriodEnd: &end, EffectiveAt: start}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{AccountUUID: user.ID, BucketStart: start.Add(time.Hour), TotalBytes: 6 * gib}); err != nil {
		t.Fatal(err)
	}

	apply := func(requestID, planID, group, pkg string, quota int64) (AdminPlanGroupChange, error) {
		t.Helper()
		current, err := st.GetUserByID(ctx, user.ID)
		if err != nil {
			return AdminPlanGroupChange{}, err
		}
		change := AdminPlanGroupChange{UserID: user.ID, PlanID: planID, SetEntitlement: true,
			ExpectedGroups: current.Groups, Groups: []string{"segment:keep", group},
			PackageName: pkg, IncludedQuotaBytes: quota, RegionMultiplier: 1, LineMultiplier: 1, PeakMultiplier: 1, OffPeakMultiplier: 1,
			PricingRuleVersion: "plan:" + planID}
		preview, err := st.CreateAdminPlanGroupPreview(ctx, AdminPlanGroupPreview{ActorUUID: "operator", Reason: "ticket #42", RequestID: requestID,
			TokenHash: "token-hash-" + requestID, ExpiresAt: time.Now().UTC().Add(10 * time.Minute), Changes: []AdminPlanGroupChange{change}})
		if err != nil {
			return AdminPlanGroupChange{}, err
		}
		if preview[0].UsedBytesPreserved != 6*gib {
			t.Fatalf("preview lost recorded usage: %d", preview[0].UsedBytesPreserved)
		}
		if err := func() error {
			_, _, err := st.ApplyAdminPlanGroupBatch(ctx, AdminPlanGroupBatch{ActorUUID: "operator", Reason: "ticket #42", RequestID: requestID, PreviewTokenHash: "token-hash-" + requestID})
			return err
		}(); err != nil {
			return AdminPlanGroupChange{}, err
		}
		return preview[0], nil
	}

	plus, err := apply("toggle-1", BillingPlanPlus, MonthlyPlusQuotaLimitGroup, "plus", 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	if plus.RemainingAfter != 14*gib {
		t.Fatalf("Plus should retain 6 GiB used, remaining=%d", plus.RemainingAfter)
	}
	free, err := apply("toggle-2", BillingPlanFree, MonthlyFreeQuotaLimitGroup, "free", 5*gib)
	if err != nil {
		t.Fatal(err)
	}
	if free.RemainingAfter != 0 {
		t.Fatalf("Free downgrade over cap should be exhausted, remaining=%d", free.RemainingAfter)
	}
	plusAgain, err := apply("toggle-3", BillingPlanPlus, MonthlyPlusQuotaLimitGroup, "plus", 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	if plusAgain.RemainingAfter != 14*gib {
		t.Fatalf("Free/Plus toggling reset usage: remaining=%d, want %d", plusAgain.RemainingAfter, 14*gib)
	}

	stored, err := st.GetAccountBillingProfile(ctx, user.ID)
	if err != nil || stored.IncludedQuotaBytes != 20*gib {
		t.Fatalf("billing profile not synchronized: %+v err=%v", stored, err)
	}
	quotaState, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil || quotaState.RemainingIncludedQuota != 14*gib {
		t.Fatalf("quota state not synchronized: %+v err=%v", quotaState, err)
	}
	reloaded, err := st.GetUserByID(ctx, user.ID)
	if err != nil || MonthlyQuotaGroup(reloaded) != MonthlyPlusQuotaLimitGroup || len(reloaded.Groups) != 2 || reloaded.Groups[0] != "segment:keep" {
		t.Fatalf("managed group update changed unrelated group tags: %+v err=%v", reloaded, err)
	}
}

func TestAdminPlanGroupBatchStaleMiddleTargetDoesNotPartiallyApply(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	users := []*User{
		{ID: "batch-a", Name: "batch a", Email: "a@example.invalid", Role: RoleUser, Groups: []string{MonthlyFreeQuotaLimitGroup}, Active: true},
		{ID: "batch-b", Name: "batch b", Email: "b@example.invalid", Role: RoleUser, Groups: []string{MonthlyFreeQuotaLimitGroup}, Active: true},
	}
	for _, user := range users {
		if err := st.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: user.ID, PackageName: "free", IncludedQuotaBytes: 5 * gib, PricingRuleVersion: "plan:FREE"}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: user.ID, RemainingIncludedQuota: 4 * gib}); err != nil {
			t.Fatal(err)
		}
	}
	changes := make([]AdminPlanGroupChange, 0, len(users))
	for _, user := range users {
		changes = append(changes, AdminPlanGroupChange{UserID: user.ID, PlanID: BillingPlanPlus, SetEntitlement: true,
			ExpectedGroups: []string{MonthlyFreeQuotaLimitGroup}, Groups: []string{MonthlyPlusQuotaLimitGroup},
			PackageName: "plus", IncludedQuotaBytes: 20 * gib, RegionMultiplier: 1, LineMultiplier: 1, PeakMultiplier: 1, OffPeakMultiplier: 1, PricingRuleVersion: "plan:PLUS"})
	}
	_, err := st.CreateAdminPlanGroupPreview(ctx, AdminPlanGroupPreview{ActorUUID: "operator", Reason: "batch ticket", RequestID: "batch-request", TokenHash: "batch-token", ExpiresAt: time.Now().UTC().Add(time.Minute), Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.GetUserByID(ctx, users[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	second.Groups = []string{"segment:changed-after-preview", MonthlyFreeQuotaLimitGroup}
	if err := st.UpdateUser(ctx, second); err != nil {
		t.Fatal(err)
	}
	_, _, err = st.ApplyAdminPlanGroupBatch(ctx, AdminPlanGroupBatch{ActorUUID: "operator", Reason: "batch ticket", RequestID: "batch-request", PreviewTokenHash: "batch-token"})
	if !errors.Is(err, ErrAdminPlanGroupStale) {
		t.Fatalf("expected stale batch rejection, got %v", err)
	}
	first, _ := st.GetUserByID(ctx, users[0].ID)
	if MonthlyQuotaGroup(first) != MonthlyFreeQuotaLimitGroup {
		t.Fatalf("first account was partially changed: groups=%v", first.Groups)
	}
	firstProfile, _ := st.GetAccountBillingProfile(ctx, users[0].ID)
	firstQuota, _ := st.GetAccountQuotaState(ctx, users[0].ID)
	if firstProfile.IncludedQuotaBytes != 5*gib || firstQuota.RemainingIncludedQuota != 4*gib {
		t.Fatalf("first account entitlement was partially changed: profile=%+v quota=%+v", firstProfile, firstQuota)
	}
	audit, err := st.ListAuditLogs(ctx, AuditLogFilter{ActionPrefix: AuditActionPlanGroupUpdate})
	if err != nil || len(audit) != 0 {
		t.Fatalf("failed batch left application audit rows: count=%d err=%v", len(audit), err)
	}
}
