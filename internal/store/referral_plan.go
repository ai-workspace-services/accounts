package store

import "strings"

func isXConnectReferralPlan(planID string) bool {
	switch strings.ToUpper(strings.TrimSpace(planID)) {
	case "PRO-MONTHLY", "PRO-YEARLY", "PRO-MONTHLY-V2", "PRO-YEARLY-V2":
		return true
	default:
		return false
	}
}

func isXConnectAnnualReferralPlan(planID string) bool {
	switch strings.ToUpper(strings.TrimSpace(planID)) {
	case "PRO-YEARLY", "PRO-YEARLY-V2":
		return true
	default:
		return false
	}
}
