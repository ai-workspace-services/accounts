package api

import (
	"account/internal/agentproto"
	"account/internal/routingpolicy"
	"account/internal/store"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

func dualBucketProGateEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("DUAL_BUCKET_PRO_GATE_ENABLED"))
	if raw == "" {
		return false
	}
	enabled, err := strconv.ParseBool(raw)
	return err == nil && enabled
}

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
	// One-time 5GB trial shares a single IncludedQuotaBytes bucket across all public pools.
	if store.IsOneTimeTrialMember(user) {
		return h.quotaExhaustedForUser(ctx, user)
	}
	if dualBucketProGateEnabled() && scope.ResourceClass == agentResourcePro {
		return h.highSpeedQuotaExhaustedForUser(ctx, user)
	}
	return h.quotaExhaustedForUser(ctx, user)
}

func (h *handler) quotaProfileAndStateForUser(ctx context.Context, user *store.User) (*store.AccountBillingProfile, *store.AccountQuotaState, bool, error) {
	if user == nil {
		return nil, nil, true, nil
	}
	// Custom-Dedicated and Media-Pool media pass-through entitlements are unlimited.
	if isCustomDedicatedUser(user, "") || hasMediaPassthroughEntitlement(user, "") {
		return nil, nil, true, nil
	}
	if _, ok := h.store.(store.NetworkTeamStore); ok {
		if access, err := h.networkAccessFor(ctx, user.ID); err == nil && access.Mode == "dedicated" && access.Allowed {
			return nil, nil, true, nil
		}
	}
	if !store.IsMonthlyQuotaLimitMember(user) {
		return nil, nil, true, nil
	}
	profile, err := h.store.GetAccountBillingProfile(ctx, user.ID)
	if errors.Is(err, store.ErrUserNotFound) {
		return nil, nil, true, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if profile == nil {
		return nil, nil, true, nil
	}
	if isCustomDedicatedUser(user, profile.PackageName) || hasMediaPassthroughEntitlement(user, profile.PackageName) {
		return nil, nil, true, nil
	}
	quota, err := h.store.GetAccountQuotaState(ctx, user.ID)
	if errors.Is(err, store.ErrUserNotFound) {
		return profile, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	return profile, quota, false, nil
}

func (h *handler) quotaExhaustedForUser(ctx context.Context, user *store.User) (bool, error) {
	profile, quota, exempt, err := h.quotaProfileAndStateForUser(ctx, user)
	if err != nil || exempt || profile == nil || profile.IncludedQuotaBytes <= 0 {
		return false, err
	}
	return quota == nil || quota.RemainingIncludedQuota <= 0, nil
}

func (h *handler) highSpeedQuotaExhaustedForUser(ctx context.Context, user *store.User) (bool, error) {
	profile, quota, exempt, err := h.quotaProfileAndStateForUser(ctx, user)
	if err != nil || exempt || profile == nil || profile.HighSpeedQuotaBytes <= 0 {
		return false, err
	}
	return quota == nil || quota.RemainingHighSpeedQuota <= 0, nil
}

func (h *handler) hasActiveHighSpeedQuotaBucket(ctx context.Context, user *store.User) (bool, error) {
	profile, _, exempt, err := h.quotaProfileAndStateForUser(ctx, user)
	if err != nil || exempt || profile == nil {
		return false, err
	}
	return profile.HighSpeedQuotaBytes > 0, nil
}

type userBucketExhaustion struct {
	gateEnabled        bool
	vpsExhausted       bool
	highSpeedExhausted bool
	hasHighSpeedBucket bool
	fullyExhausted     bool
}

func (h *handler) evaluateDualBucketQuotaForUser(ctx context.Context, user *store.User) (userBucketExhaustion, error) {
	status := userBucketExhaustion{
		gateEnabled: dualBucketProGateEnabled(),
	}
	vpsExhausted, err := h.quotaExhaustedForUser(ctx, user)
	if err != nil {
		return status, err
	}
	status.vpsExhausted = vpsExhausted
	if store.IsOneTimeTrialMember(user) {
		status.hasHighSpeedBucket = false
		status.highSpeedExhausted = vpsExhausted
		status.fullyExhausted = vpsExhausted
		return status, nil
	}
	if !status.gateEnabled {
		status.fullyExhausted = vpsExhausted
		return status, nil
	}
	hasHighSpeed, err := h.hasActiveHighSpeedQuotaBucket(ctx, user)
	if err != nil {
		return status, err
	}
	status.hasHighSpeedBucket = hasHighSpeed
	if hasHighSpeed {
		hsExhausted, err := h.highSpeedQuotaExhaustedForUser(ctx, user)
		if err != nil {
			return status, err
		}
		status.highSpeedExhausted = hsExhausted
	}
	status.fullyExhausted = vpsExhausted && (!hasHighSpeed || status.highSpeedExhausted)
	return status, nil
}

func poolAllowedForQuota(routingPool string, status userBucketExhaustion) bool {
	if !status.gateEnabled {
		return true
	}
	switch strings.TrimSpace(routingPool) {
	case routingpolicy.RoutingPoolProEC2:
		return !status.highSpeedExhausted
	case routingpolicy.RoutingPoolMediaPriority:
		return !status.vpsExhausted
	default:
		return true
	}
}
