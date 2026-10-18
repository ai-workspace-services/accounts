package store

import "testing"

func TestVersionedXConnectPlansRemainValidReferralProducts(t *testing.T) {
	for _, planID := range []string{"PRO-MONTHLY", "PRO-MONTHLY-V2", "PRO-YEARLY", "PRO-YEARLY-V2"} {
		if !isXConnectReferralPlan(planID) {
			t.Errorf("plan %s should remain eligible for referral accounting", planID)
		}
	}
	for _, planID := range []string{"PRO-YEARLY", "PRO-YEARLY-V2"} {
		if !isXConnectAnnualReferralPlan(planID) {
			t.Errorf("plan %s should be treated as an annual referral plan", planID)
		}
	}
}
