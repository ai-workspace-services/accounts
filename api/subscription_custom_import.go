package api

import (
	"account/internal/store"
	"context"
	"errors"
	"strings"
	"time"
)

func (h *handler) customSubscriptionImport(ctx context.Context, u *store.User, update adminPlanGroupUpdate) (store.AdminPlanGroupChange, error) {
	p, err := h.store.GetAccountBillingProfile(ctx, u.ID)
	if err != nil {
		return store.AdminPlanGroupChange{}, err
	}
	if p == nil || update.SubscriptionRecord.MonthlyQuotaBytes <= 0 {
		return store.AdminPlanGroupChange{}, errors.New("custom import needs an existing billing profile and positive monthly quota")
	}
	change := store.AdminPlanGroupChange{UserID: u.ID, PlanID: existingPlanID(p), SetEntitlement: true, Groups: normalizeGroups(store.WithSubscriptionSourcePlan(u.Groups, "CUSTOM")), PackageName: p.PackageName, IncludedQuotaBytes: update.SubscriptionRecord.MonthlyQuotaBytes, RegionMultiplier: p.RegionMultiplier, LineMultiplier: p.LineMultiplier, PeakMultiplier: p.PeakMultiplier, OffPeakMultiplier: p.OffPeakMultiplier, PricingRuleVersion: p.PricingRuleVersion, SubscriptionRecord: update.SubscriptionRecord}
	if change.PlanID == "" {
		change.PlanID = "CUSTOM"
	}
	err = setPlanValidity(&change, u, update)
	if err != nil {
		return change, err
	}
	effective := *u
	if change.SetValidFrom {
		effective.SubscriptionValidFrom = change.ValidFrom
	}
	if change.SetValidUntil {
		effective.SubscriptionValidUntil = change.ValidUntil
	}
	now := time.Now().UTC()
	payment := strings.ToLower(strings.TrimSpace(update.SubscriptionRecord.PaymentStatus))
	unpaid := payment != "" && payment != "paid" && payment != "已付费" && payment != "已支付"
	if unpaid || subscriptionValidityExpired(&effective, now) || effective.SubscriptionValidFrom != nil && now.Before(*effective.SubscriptionValidFrom) {
		change.SetEntitlement = false
		change.IncludedQuotaBytes = p.IncludedQuotaBytes
	}
	return change, nil
}
