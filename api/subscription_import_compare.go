package api

import (
	"account/internal/store"
	"context"
	"errors"
	"reflect"
	"sort"
	"time"
)

func sameImportTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}
func (h *handler) subscriptionImportUnchanged(ctx context.Context, c store.AdminPlanGroupChange) (bool, error) {
	u, err := h.store.GetUserByID(ctx, c.UserID)
	if err != nil {
		return false, err
	}
	p, err := h.store.GetAccountBillingProfile(ctx, c.UserID)
	if errors.Is(err, store.ErrUserNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	r, err := h.store.GetSubscriptionRecord(ctx, c.UserID)
	if errors.Is(err, store.ErrUserNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r == nil || c.SubscriptionRecord == nil {
		return false, nil
	}
	a, b := append([]string{}, u.Groups...), append([]string{}, c.Groups...)
	sort.Strings(a)
	sort.Strings(b)
	same := reflect.DeepEqual(a, b) && p.PackageName == c.PackageName && p.IncludedQuotaBytes == c.IncludedQuotaBytes && p.PricingRuleVersion == c.PricingRuleVersion && p.RegionMultiplier == c.RegionMultiplier && p.LineMultiplier == c.LineMultiplier && p.PeakMultiplier == c.PeakMultiplier && p.OffPeakMultiplier == c.OffPeakMultiplier
	if c.SetValidFrom {
		same = same && sameImportTime(u.SubscriptionValidFrom, c.ValidFrom)
	}
	if c.SetValidUntil {
		same = same && sameImportTime(u.SubscriptionValidUntil, c.ValidUntil)
	}
	want := c.SubscriptionRecord
	return same && r.PlanCode == want.PlanCode && r.SubscriptionType == want.SubscriptionType && r.MonthlyQuotaBytes == want.MonthlyQuotaBytes && r.AnnualQuotaBytes == want.AnnualQuotaBytes && r.FeeMinor == want.FeeMinor && r.PaymentStatus == want.PaymentStatus && r.Note == want.Note && sameImportTime(r.ValidFrom, want.ValidFrom) && sameImportTime(r.ValidUntil, want.ValidUntil), nil
}
