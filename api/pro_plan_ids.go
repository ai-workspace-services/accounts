package api

import "strings"

func isXConnectProPlanID(planID string) bool {
	switch strings.ToUpper(strings.TrimSpace(planID)) {
	case "PRO-MONTHLY", "PRO-YEARLY", "PRO-MONTHLY-V2", "PRO-YEARLY-V2":
		return true
	default:
		return false
	}
}

func isXConnectAnnualPlanID(planID string) bool {
	switch strings.ToUpper(strings.TrimSpace(planID)) {
	case "PRO-YEARLY", "PRO-YEARLY-V2":
		return true
	default:
		return false
	}
}
