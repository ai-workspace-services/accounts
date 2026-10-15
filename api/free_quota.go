package api

import (
	"account/internal/store"
	"context"
	"errors"
	"time"
)

// Calendar month uses UTC consistently with accounting period and CSV dates.
// The store uses a conditional update, so replicas cannot grant twice.
func (h *handler) ensureFreeQuotaPeriod(ctx context.Context, user *store.User, now time.Time) error {
	if user == nil || !store.IsMonthlyFreeQuotaLimitMember(user) {
		return nil
	}
	start, end := naturalMonthPeriod(now)
	return h.store.ResetFreeQuotaPeriod(ctx, user.ID, start, end, now.UTC())
}

func (h *handler) quotaExhaustedForUser(ctx context.Context, user *store.User) (bool, error) {
	if !store.IsMonthlyQuotaLimitMember(user) {
		return false, nil
	}
	profile, err := h.store.GetAccountBillingProfile(ctx, user.ID)
	if errors.Is(err, store.ErrUserNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if profile == nil || profile.IncludedQuotaBytes <= 0 {
		return false, nil
	}
	quota, err := h.store.GetAccountQuotaState(ctx, user.ID)
	if errors.Is(err, store.ErrUserNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return quota == nil || quota.RemainingIncludedQuota <= 0, nil
}
