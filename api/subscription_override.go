package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"account/internal/store"
)

func planChangeQuotaPaused(change store.AdminPlanGroupChange) bool {
	from, until := change.ExpectedValidFrom, change.ExpectedValidUntil
	if change.SetValidFrom {
		from = change.ValidFrom
	}
	if change.SetValidUntil {
		until = change.ValidUntil
	}
	u := &store.User{Active: true, Groups: change.Groups, SubscriptionValidFrom: from, SubscriptionValidUntil: until}
	return change.IncludedQuotaBytes > 0 && change.RemainingAfter == 0 && !store.HasSubscriptionUnlimitedOverride(u, time.Now())
}

// Group-only previews already persist an audited, optimistic transaction without
// rewriting plan/profile/quota/validity rows. Removing this marker restores quota
// enforcement immediately, including any usage accumulated during the override.
func (h *handler) validateSubscriptionOverride(ctx context.Context, actor, user *store.User, groups []string) error {
	before := slices.Contains(user.Groups, store.SubscriptionUnlimitedOverrideGroup)
	after := slices.Contains(groups, store.SubscriptionUnlimitedOverrideGroup)
	if before == after {
		return nil
	}
	if !store.IsRootRole(actor.Role) {
		return errors.New("only root can change the temporary unlimited override")
	}
	if !after {
		return nil
	}
	if !slices.Contains(groups, store.SubscriptionListedGroup) {
		return errors.New("temporary unlimited requires current CSV cohort membership")
	}
	record, err := h.store.GetSubscriptionRecord(ctx, user.ID)
	if err != nil || record == nil {
		return errors.New("a persisted monthly or annual subscription is required")
	}
	paid := strings.ToLower(strings.TrimSpace(record.PaymentStatus))
	if record.SubscriptionType != "month" && record.SubscriptionType != "year" || paid != "paid" && paid != "已付费" && paid != "已支付" {
		return errors.New("temporary unlimited requires a paid monthly or annual subscription")
	}
	if isTeamSubscriptionUser(user.Groups) {
		return errors.New("team entitlements require team management")
	}
	return nil
}
