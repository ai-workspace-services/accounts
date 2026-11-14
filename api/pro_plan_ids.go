package api

import "strings"

func isXConnectProPlanID(planID string) bool {
	upper := strings.ToUpper(strings.TrimSpace(planID))
	return upper == "PRO" || strings.HasPrefix(upper, "PRO-")
}

func isXConnectAnnualPlanID(planID string) bool {
	upper := strings.ToUpper(strings.TrimSpace(planID))
	return strings.Contains(upper, "YEARLY") || strings.Contains(upper, "ANNUAL")
}
