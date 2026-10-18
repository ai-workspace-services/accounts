package api

import "testing"

func TestVersionedProPlanIDsPreserveReferralPlanType(t *testing.T) {
	for _, planID := range []string{"PRO-MONTHLY", "PRO-MONTHLY-V2"} {
		if !isXConnectProPlanID(planID) || isXConnectAnnualPlanID(planID) {
			t.Errorf("monthly plan classification is incorrect for %s", planID)
		}
	}
	for _, planID := range []string{"PRO-YEARLY", "PRO-YEARLY-V2"} {
		if !isXConnectProPlanID(planID) || !isXConnectAnnualPlanID(planID) {
			t.Errorf("annual plan classification is incorrect for %s", planID)
		}
	}
}
