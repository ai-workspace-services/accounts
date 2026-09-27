package api

import (
	"context"
	"errors"
	"strings"

	"account/internal/store"
)

type accountPlanContractPayload struct {
	PlanID                 string `json:"planId,omitempty"`
	DisplayName            string `json:"displayName"`
	PackageName            string `json:"packageName"`
	MaxTrafficBytes        int64  `json:"maxTrafficBytes"`
	CatalogMaxTrafficBytes int64  `json:"catalogMaxTrafficBytes,omitempty"`
	QuotaCycle             string `json:"quotaCycle,omitempty"`
	Unlimited              bool   `json:"unlimited"`
	Assigned               bool   `json:"assigned"`
	Source                 string `json:"source"`
}

type accountPlanContract struct {
	CurrentPlan      *accountPlanContractPayload
	DefaultPlan      *accountPlanContractPayload
	AssignmentStatus string
}

// accountPlanContract resolves display metadata from the local plan catalog.
// The account billing profile remains the effective assigned allowance used
// by metering; the default plan is returned as reference data only and never
// creates or changes an account assignment.
func (h *handler) accountPlanContract(ctx context.Context, profile *store.AccountBillingProfile) (accountPlanContract, error) {
	result := accountPlanContract{AssignmentStatus: "unassigned"}
	defaultPlan, err := h.store.GetBillingPlan(ctx, store.BillingPlanFree)
	if err != nil {
		if !errors.Is(err, store.ErrBillingPlanNotFound) {
			return result, err
		}
	} else {
		result.DefaultPlan = planContractFromCatalog(defaultPlan, 0, false, "local_catalog_default")
	}

	if profile == nil {
		return result, nil
	}

	var catalogPlan *store.BillingPlan
	version := strings.TrimSpace(profile.PricingRuleVersion)
	if strings.HasPrefix(version, "plan:") {
		catalogPlan, err = h.store.GetBillingPlan(ctx, strings.TrimPrefix(version, "plan:"))
		if err != nil && !errors.Is(err, store.ErrBillingPlanNotFound) {
			return result, err
		}
	}
	if catalogPlan == nil {
		plans, listErr := h.store.ListBillingPlans(ctx, true)
		if listErr != nil {
			return result, listErr
		}
		for i := range plans {
			if strings.EqualFold(strings.TrimSpace(plans[i].PackageName), strings.TrimSpace(profile.PackageName)) {
				catalogPlan = &plans[i]
				break
			}
		}
	}

	if catalogPlan != nil {
		result.AssignmentStatus = "assigned"
		result.CurrentPlan = planContractFromCatalog(catalogPlan, profile.IncludedQuotaBytes, true, "account_entitlement")
		return result, nil
	}

	packageName := strings.TrimSpace(profile.PackageName)
	if packageName == "" {
		packageName = "default"
	}
	if strings.EqualFold(packageName, "default") {
		// Legacy profiles named "default" predate an explicit local plan
		// assignment. Keep their stored quota untouched and expose FREE only as
		// the separate, unassigned catalog reference above.
		return result, nil
	}
	result.AssignmentStatus = "assigned"
	result.CurrentPlan = &accountPlanContractPayload{
		DisplayName:     packageName,
		PackageName:     packageName,
		MaxTrafficBytes: profile.IncludedQuotaBytes,
		Assigned:        true,
		Source:          "account_billing_profile",
	}
	return result, nil
}

func planContractFromCatalog(plan *store.BillingPlan, effectiveQuota int64, assigned bool, source string) *accountPlanContractPayload {
	if plan == nil {
		return nil
	}
	maxTraffic := plan.IncludedQuotaBytes
	if assigned {
		// The profile is the exact allowance currently enforced for this
		// assignment. Catalog edits become effective on the next assignment.
		maxTraffic = effectiveQuota
	}
	quotaCycle, _ := plan.Features["quota_cycle"].(string)
	fastLane, _ := plan.Features["fast_lane"].(map[string]any)
	mode, _ := fastLane["mode"].(string)
	return &accountPlanContractPayload{
		PlanID:                 plan.PlanID,
		DisplayName:            plan.DisplayName,
		PackageName:            plan.PackageName,
		MaxTrafficBytes:        maxTraffic,
		CatalogMaxTrafficBytes: plan.IncludedQuotaBytes,
		QuotaCycle:             quotaCycle,
		Unlimited:              mode == "unlimited",
		Assigned:               assigned,
		Source:                 source,
	}
}
