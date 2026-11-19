package api

import (
	"account/internal/agentproto"
	"account/internal/store"
	"context"
	"errors"
	"strings"
	"time"
)

func hasMediaPassthroughEntitlement(user *store.User, packageName string) bool {
	pkg := strings.ToLower(strings.TrimSpace(packageName))
	if pkg == "media-passthrough" || pkg == "unlimited" || pkg == "unlimited-beta" {
		return true
	}
	if user == nil {
		return false
	}
	for _, group := range user.Groups {
		switch strings.ToLower(strings.TrimSpace(group)) {
		case "media:passthrough", "segment:media-passthrough", "pool:media-passthrough", "segment:quota:unlimited":
			return true
		}
	}
	return false
}

func hasMediaPassthroughAgentGroup(groups []string) bool {
	for _, group := range groups {
		switch strings.ToLower(strings.TrimSpace(group)) {
		case "media:passthrough", "pool:media-passthrough":
			return true
		}
	}
	return false
}

// Calendar month uses UTC consistently with accounting period and CSV dates.
// The store uses a conditional update, so replicas cannot grant twice.
func (h *handler) ensureFreeQuotaPeriod(ctx context.Context, user *store.User, now time.Time) error {
	if user == nil || !store.IsMonthlyFreeQuotaLimitMember(user) {
		return nil
	}
	start, end := naturalMonthPeriod(now)
	return h.store.ResetFreeQuotaPeriod(ctx, user.ID, start, end, now.UTC())
}

func (h *handler) quotaExhaustedForAgentScope(ctx context.Context, user *store.User, scope agentproto.UserScope, groups []string) (bool, error) {
	if scope.ResourceClass == agentResourceCustom || scope.ResourceClass == agentResourceTeam {
		return false, nil
	}
	if hasMediaPassthroughAgentGroup(groups) {
		return false, nil
	}
	return h.quotaExhaustedForUser(ctx, user)
}

func (h *handler) quotaExhaustedForUser(ctx context.Context, user *store.User) (bool, error) {
	if user == nil {
		return false, nil
	}
	// Custom-Dedicated and Media-Pool media pass-through entitlements are unlimited.
	if isCustomDedicatedUser(user, "") || hasMediaPassthroughEntitlement(user, "") {
		return false, nil
	}
	if _, ok := h.store.(store.NetworkTeamStore); ok {
		if access, err := h.networkAccessFor(ctx, user.ID); err == nil && access.Mode == "dedicated" && access.Allowed {
			return false, nil
		}
	}
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
	if isCustomDedicatedUser(user, profile.PackageName) || hasMediaPassthroughEntitlement(user, profile.PackageName) {
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
