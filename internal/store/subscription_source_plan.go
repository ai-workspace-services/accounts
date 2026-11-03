package store

import "strings"

// Non-entitlement provenance stored in the existing groups JSON. It never
// selects nodes or grants access; the billing profile and access groups do.
const subscriptionSourcePlanPrefix = "subscription:source-plan:"

func SubscriptionSourcePlan(groups []string) string {
	for _, g := range groups {
		if strings.HasPrefix(g, subscriptionSourcePlanPrefix) {
			return strings.ToUpper(strings.TrimPrefix(g, subscriptionSourcePlanPrefix))
		}
	}
	return ""
}
func WithSubscriptionSourcePlan(groups []string, plan string) []string {
	out := make([]string, 0, len(groups)+1)
	for _, g := range groups {
		if !strings.HasPrefix(g, subscriptionSourcePlanPrefix) {
			out = append(out, g)
		}
	}
	if plan != "" {
		out = append(out, subscriptionSourcePlanPrefix+strings.ToLower(plan))
	}
	return out
}
