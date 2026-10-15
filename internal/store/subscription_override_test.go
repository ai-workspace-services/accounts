package store

import (
	"testing"
	"time"
)

func TestSubscriptionUnlimitedOverrideLeavesQuotaTierAndValidityIntact(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	user := &User{Active: true, Groups: []string{MonthlyPlusQuotaLimitGroup, "segment:yearly", SubscriptionListedGroup, SubscriptionUnlimitedOverrideGroup}, SubscriptionValidUntil: &until}
	if !HasSubscriptionUnlimitedOverride(user, now) || MonthlyQuotaGroup(user) != MonthlyPlusQuotaLimitGroup {
		t.Fatal("override must retain plus quota tier")
	}
	if HasSubscriptionUnlimitedOverride(user, now.Add(24*time.Hour)) {
		t.Fatal("override extended an expired subscription")
	}
	user.SubscriptionValidUntil = nil
	if !HasSubscriptionUnlimitedOverride(user, now) {
		t.Fatal("missing operator dates must not be fabricated")
	}
	for _, groups := range [][]string{{MonthlyFreeQuotaLimitGroup, SubscriptionUnlimitedOverrideGroup}, {MonthlyPlusQuotaLimitGroup, "segment:payg", SubscriptionUnlimitedOverrideGroup}, {MonthlyUnlimitedBetaQuotaGroup, SubscriptionUnlimitedOverrideGroup}, {MonthlyPlusQuotaLimitGroup, "segment:yearly"}} {
		user.Groups = groups
		if HasSubscriptionUnlimitedOverride(user, now) {
			t.Fatalf("unrelated membership granted unlimited: %v", groups)
		}
	}
}
