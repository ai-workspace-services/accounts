package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

type whitelistGroupMemberPayload struct {
	UserID                 string `json:"userId"`
	Email                  string `json:"email"`
	Name                   string `json:"name"`
	Role                   string `json:"role"`
	RemainingIncludedQuota int64  `json:"remainingIncludedQuota"`
	IncludedQuotaBytes     int64  `json:"includedQuotaBytes"`
	ProxyAccessState       string `json:"proxyAccessState"`
	HasPaidSubscription    bool   `json:"hasPaidSubscription"`
	CurrentPlanID          string `json:"currentPlanId,omitempty"`
}

type whitelistGroupConfigPayload struct {
	GroupID           string                        `json:"groupId"`
	DisplayName       string                        `json:"displayName"`
	MonthlyQuotaBytes int64                         `json:"monthlyQuotaBytes"`
	AllowedPools      []string                      `json:"allowedPools"`
	Active            bool                          `json:"active"`
	Description       string                        `json:"description,omitempty"`
	Members           []whitelistGroupMemberPayload `json:"members"`
}

type updateWhitelistGroupRequest struct {
	DisplayName       string   `json:"displayName"`
	MonthlyQuotaBytes *int64   `json:"monthlyQuotaBytes"`
	AllowedPools      []string `json:"allowedPools"`
	Active            *bool    `json:"active"`
	Description       string   `json:"description"`
	Reason            string   `json:"reason"`
}

type addWhitelistGroupMemberRequest struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

type removeWhitelistGroupMemberRequest struct {
	Reason string `json:"reason"`
}

func whitelistAllowedPools(plan *store.BillingPlan) []string {
	if plan != nil && plan.Features != nil {
		if raw, ok := plan.Features["allowed_pools"].([]string); ok && len(raw) > 0 {
			return append([]string(nil), raw...)
		}
		if rawAny, ok := plan.Features["allowed_pools"].([]any); ok && len(rawAny) > 0 {
			out := make([]string, 0, len(rawAny))
			for _, item := range rawAny {
				if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
					out = append(out, strings.TrimSpace(text))
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return []string{"Media-Pool"}
}

func whitelistDescription(plan *store.BillingPlan) string {
	if plan != nil && plan.Features != nil {
		if desc, ok := plan.Features["description"].(string); ok {
			return strings.TrimSpace(desc)
		}
	}
	return ""
}

func (h *handler) hasActiveCommercialEntitlement(ctx context.Context, user *store.User) bool {
	if user == nil {
		return false
	}
	if h.hasActivePaidSubscription(ctx, user.ID) {
		return true
	}
	if store.SubscriptionSourcePlan(user.Groups) == "CUSTOM" || store.HasSubscriptionUnlimitedOverride(user, time.Now().UTC()) {
		return true
	}
	if teamStore, ok := h.store.(store.NetworkTeamStore); ok {
		if membership, err := teamStore.GetNetworkMembership(ctx, user.ID); err == nil && membership.TeamID != "" && membership.DedicatedOnly {
			return true
		}
	}
	profile, err := h.store.GetAccountBillingProfile(ctx, user.ID)
	if err == nil && profile != nil {
		group := store.MonthlyQuotaGroup(user)
		switch strings.ToLower(strings.TrimSpace(profile.PackageName)) {
		case "plus", "pro", "custom", "unlimited":
			if group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup {
				return true
			}
		}
	}
	return false
}

func (h *handler) buildWhitelistGroupPayload(ctx context.Context) (whitelistGroupConfigPayload, error) {
	plan := h.resolveWhitelistFreePlan(ctx)
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		return whitelistGroupConfigPayload{}, err
	}
	members := make([]whitelistGroupMemberPayload, 0)
	for i := range users {
		u := &users[i]
		if !store.IsWhitelistFreeMember(u) {
			continue
		}
		member := whitelistGroupMemberPayload{
			UserID:           u.ID,
			Email:            u.Email,
			Name:             u.Name,
			Role:             u.Role,
			ProxyAccessState: "active",
		}
		if profile, pErr := h.store.GetAccountBillingProfile(ctx, u.ID); pErr == nil && profile != nil {
			member.IncludedQuotaBytes = profile.IncludedQuotaBytes
			member.CurrentPlanID = strings.TrimPrefix(strings.TrimSpace(profile.PricingRuleVersion), "plan:")
		}
		if state, qErr := h.store.GetAccountQuotaState(ctx, u.ID); qErr == nil && state != nil {
			member.RemainingIncludedQuota = state.RemainingIncludedQuota
			if strings.TrimSpace(state.ProxyAccessState) != "" {
				member.ProxyAccessState = state.ProxyAccessState
			}
		}
		member.HasPaidSubscription = h.hasActiveCommercialEntitlement(ctx, u)
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool {
		return strings.ToLower(members[i].Email) < strings.ToLower(members[j].Email)
	})
	return whitelistGroupConfigPayload{
		GroupID:           store.BillingPlanWhitelistFree,
		DisplayName:       plan.DisplayName,
		MonthlyQuotaBytes: plan.IncludedQuotaBytes,
		AllowedPools:      whitelistAllowedPools(plan),
		Active:            plan.Active,
		Description:       whitelistDescription(plan),
		Members:           members,
	}, nil
}

func (h *handler) adminListWhitelistGroups(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsRead); !ok {
		return
	}
	group, err := h.buildWhitelistGroupPayload(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_groups_unavailable", "failed to load whitelist groups")
		return
	}
	c.JSON(http.StatusOK, gin.H{"groups": []whitelistGroupConfigPayload{group}})
}

func normalizeWhitelistGroupID(raw string) (string, bool) {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == store.BillingPlanWhitelistFree {
		return store.BillingPlanWhitelistFree, true
	}
	return "", false
}

func (h *handler) adminUpdateWhitelistGroup(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	groupID, valid := normalizeWhitelistGroupID(c.Param("groupId"))
	if !valid {
		respondError(c, http.StatusNotFound, "whitelist_group_not_found", "whitelist group not found")
		return
	}
	var req updateWhitelistGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid whitelist group payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	current := h.resolveWhitelistFreePlan(ctx)
	before := map[string]any{
		"display_name":         current.DisplayName,
		"included_quota_bytes": current.IncludedQuotaBytes,
		"allowed_pools":        whitelistAllowedPools(current),
		"active":               current.Active,
		"description":          whitelistDescription(current),
	}

	next := *current
	features := make(map[string]any, len(current.Features)+4)
	for k, v := range current.Features {
		features[k] = v
	}
	next.Features = features
	next.PlanID = groupID
	next.Kind = "whitelist_group"
	next.PackageName = "whitelist"
	next.HighSpeedQuotaBytes = 0
	if name := strings.TrimSpace(req.DisplayName); name != "" {
		next.DisplayName = name
	}
	if req.MonthlyQuotaBytes != nil {
		if *req.MonthlyQuotaBytes < 0 {
			respondError(c, http.StatusBadRequest, "invalid_request", "monthlyQuotaBytes must be non-negative")
			return
		}
		next.IncludedQuotaBytes = *req.MonthlyQuotaBytes
	}
	if req.AllowedPools != nil {
		cleaned := make([]string, 0, len(req.AllowedPools))
		for _, pool := range req.AllowedPools {
			if trimmed := strings.TrimSpace(pool); trimmed != "" {
				cleaned = append(cleaned, trimmed)
			}
		}
		if len(cleaned) == 0 {
			cleaned = []string{"Media-Pool"}
		}
		next.Features["allowed_pools"] = cleaned
	}
	if req.Active != nil {
		next.Active = *req.Active
	}
	if desc := strings.TrimSpace(req.Description); desc != "" {
		next.Features["description"] = desc
	}
	next.Features["internal_only"] = true
	next.Features["family"] = "whitelist"
	next.Features["tier_rank"] = 0
	next.Features = enrichBillingPlanFeatures(&next)

	if err := h.store.UpsertBillingPlan(ctx, &next); err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_group_save_failed", "failed to save whitelist group")
		return
	}
	after := map[string]any{
		"display_name":         next.DisplayName,
		"included_quota_bytes": next.IncludedQuotaBytes,
		"allowed_pools":        whitelistAllowedPools(&next),
		"active":               next.Active,
		"description":          whitelistDescription(&next),
	}
	if err := h.recordAudit(ctx, actor.ID, store.AuditActionPlanUpsert, auditDetails(groupID, reason, before, after)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "whitelist group was updated but audit entry failed")
		return
	}
	group, err := h.buildWhitelistGroupPayload(ctx)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_groups_unavailable", "failed to reload whitelist group")
		return
	}
	c.JSON(http.StatusOK, gin.H{"group": group})
}

func (h *handler) adminAddWhitelistGroupMember(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	groupID, valid := normalizeWhitelistGroupID(c.Param("groupId"))
	if !valid {
		respondError(c, http.StatusNotFound, "whitelist_group_not_found", "whitelist group not found")
		return
	}
	var req addWhitelistGroupMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		respondError(c, http.StatusBadRequest, "email_required", "email is required")
		return
	}
	ctx := c.Request.Context()
	user, err := h.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to find user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusBadRequest, "root_not_eligible", "root account cannot be added to whitelist group")
		return
	}

	beforeGroups := append([]string(nil), user.Groups...)
	nextGroups := make([]string, 0, len(user.Groups)+2)
	for _, g := range user.Groups {
		switch strings.TrimSpace(g) {
		case store.OneTimeTrialQuotaGroup, store.MonthlyFreeQuotaLimitGroup, "segment:free", store.WhitelistFreeQuotaGroup:
			continue
		default:
			nextGroups = append(nextGroups, g)
		}
	}
	nextGroups = append(nextGroups, store.WhitelistFreeQuotaGroup)
	if !h.hasActiveCommercialEntitlement(ctx, user) {
		nextGroups = replaceMonthlyQuotaGroup(nextGroups, store.WhitelistFreeQuotaGroup)
	}
	user.Groups = normalizeGroups(nextGroups)
	if err := h.store.UpdateUser(ctx, user); err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_member_save_failed", "failed to update user groups")
		return
	}

	if !h.hasActiveCommercialEntitlement(ctx, user) {
		plan := h.resolveWhitelistFreePlan(ctx)
		if err := h.applyPlanEntitlements(ctx, user.ID, plan); err != nil {
			respondError(c, http.StatusInternalServerError, "entitlement_apply_failed", "failed to apply whitelist entitlement")
			return
		}
		now := time.Now().UTC()
		periodStart, periodEnd := naturalMonthPeriod(now)
		existingProfile, _ := h.store.GetAccountBillingProfile(ctx, user.ID)
		existingQuota, _ := h.store.GetAccountQuotaState(ctx, user.ID)
		remaining := plan.IncludedQuotaBytes
		if existingProfile != nil && existingQuota != nil && strings.EqualFold(existingProfile.PackageName, "whitelist") &&
			existingQuota.PeriodStart != nil && existingQuota.PeriodEnd != nil &&
			existingQuota.PeriodStart.Equal(periodStart) && existingQuota.PeriodEnd.Equal(periodEnd) {
			remaining = existingQuota.RemainingIncludedQuota
			if remaining > plan.IncludedQuotaBytes {
				remaining = plan.IncludedQuotaBytes
			}
			if remaining < 0 {
				remaining = 0
			}
		}
		state := existingQuota
		if state == nil {
			state = &store.AccountQuotaState{AccountUUID: user.ID}
		}
		state.RemainingIncludedQuota = remaining
		state.RemainingHighSpeedQuota = 0
		state.Arrears = false
		state.ArrearsSince = nil
		state.ThrottleState = "normal"
		if state.SuspendState != "suspended" {
			state.SuspendState = "active"
		}
		if remaining > 0 {
			state.ProxyAccessState = "active"
		} else {
			state.ProxyAccessState = "paused"
		}
		state.PeriodStart = &periodStart
		state.PeriodEnd = &periodEnd
		state.EffectiveAt = now
		if err := h.store.UpsertAccountQuotaState(ctx, state); err != nil {
			respondError(c, http.StatusInternalServerError, "quota_update_failed", "failed to update whitelist quota")
			return
		}
	}

	if err := h.recordAudit(ctx, actor.ID, store.AuditActionSegmentUpdate, auditDetails(user.ID, reason,
		map[string]any{"group_id": groupID, "groups": beforeGroups},
		map[string]any{"group_id": groupID, "groups": user.Groups},
	)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "member added but audit write failed")
		return
	}
	group, err := h.buildWhitelistGroupPayload(ctx)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_groups_unavailable", "failed to reload whitelist group")
		return
	}
	c.JSON(http.StatusOK, gin.H{"group": group})
}

func (h *handler) adminRemoveWhitelistGroupMember(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	groupID, valid := normalizeWhitelistGroupID(c.Param("groupId"))
	if !valid {
		respondError(c, http.StatusNotFound, "whitelist_group_not_found", "whitelist group not found")
		return
	}
	userID := strings.TrimSpace(c.Param("userId"))
	if userID == "" {
		respondError(c, http.StatusBadRequest, "user_id_required", "userId is required")
		return
	}
	rawReason := strings.TrimSpace(c.Query("reason"))
	if rawReason == "" {
		var body removeWhitelistGroupMemberRequest
		if c.Request.ContentLength > 0 {
			_ = c.ShouldBindJSON(&body)
			rawReason = body.Reason
		}
	}
	reason, ok := requireReason(c, rawReason)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	user, err := h.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to load user")
		return
	}

	beforeGroups := append([]string(nil), user.Groups...)
	nextGroups := make([]string, 0, len(user.Groups))
	for _, g := range user.Groups {
		switch strings.TrimSpace(g) {
		case store.WhitelistFreeQuotaGroup, store.MonthlyFreeQuotaLimitGroup:
			continue
		default:
			nextGroups = append(nextGroups, g)
		}
	}
	user.Groups = normalizeGroups(nextGroups)
	if err := h.store.UpdateUser(ctx, user); err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_member_remove_failed", "failed to remove user from whitelist group")
		return
	}

	if !h.hasActiveCommercialEntitlement(ctx, user) {
		if err := h.pauseTrafficOrFallbackWhitelist(ctx, user.ID); err != nil {
			respondError(c, http.StatusInternalServerError, "whitelist_pause_failed", "failed to pause removed whitelist user traffic")
			return
		}
	}
	if err := h.recordAudit(ctx, actor.ID, store.AuditActionSegmentUpdate, auditDetails(user.ID, reason,
		map[string]any{"group_id": groupID, "groups": beforeGroups},
		map[string]any{"group_id": groupID, "groups": user.Groups},
	)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "member removed but audit write failed")
		return
	}
	group, err := h.buildWhitelistGroupPayload(ctx)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "whitelist_groups_unavailable", "failed to reload whitelist group")
		return
	}
	c.JSON(http.StatusOK, gin.H{"group": group})
}
