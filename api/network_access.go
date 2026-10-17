package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

const (
	agentResourceGroupPrefix = "resource:"
	agentResourceShared      = "shared"
	agentResourceFree        = "free"
	agentResourcePlus        = "plus"
)

// agentResourceScope resolves the pool entitlement from the enrolled
// credential. A team marker remains the source of tenant isolation; a
// resource marker selects a public pool. Conflicting markers fail closed.
func agentResourceScope(groups []string) (agentproto.UserScope, bool) {
	scope := agentproto.UserScope{ResourceClass: agentResourceShared}
	seen := ""
	team, valid := agentNetworkScope(groups)
	if !valid {
		return agentproto.UserScope{}, false
	}
	if team != "" {
		scope.ResourceClass, scope.TeamID = "team", team
		seen = "team"
	}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if !strings.HasPrefix(group, agentResourceGroupPrefix) {
			continue
		}
		class := strings.TrimPrefix(group, agentResourceGroupPrefix)
		if class != agentResourceShared && class != agentResourceFree && class != agentResourcePlus {
			return agentproto.UserScope{}, false
		}
		if seen != "" && seen != class {
			return agentproto.UserScope{}, false
		}
		seen = class
		scope.ResourceClass = class
	}
	if scope.ResourceClass == "team" && seen != "team" {
		return agentproto.UserScope{}, false
	}
	if scope.ResourceClass != "team" && scope.TeamID != "" {
		return agentproto.UserScope{}, false
	}
	return scope, true
}

func agentResourceScopeAllowsUser(scope agentproto.UserScope, access networkAccess, user *store.User) bool {
	if scope.ResourceClass == "team" {
		return access.Mode == "dedicated" && access.TeamID == scope.TeamID
	}
	if access.Mode == "dedicated" {
		return false
	}
	switch scope.ResourceClass {
	case agentResourceFree:
		// Free pools accept registered Free users and personal Plus users.
		group := store.MonthlyQuotaGroup(user)
		return group == "" || group == store.MonthlyFreeQuotaLimitGroup ||
			group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup
	case agentResourcePlus:
		group := store.MonthlyQuotaGroup(user)
		return group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup
	default:
		return true
	}
}

const (
	teamAdminGroupPrefix = "team-admin:"
	teamAdminRoleSuffix  = ":admin"
)

var networkTeamIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type networkAccess struct {
	AccountID  string             `json:"accountId,omitempty"`
	Configured bool               `json:"-"`
	Mode       string             `json:"mode"`
	TeamID     string             `json:"teamId,omitempty"`
	Team       *store.NetworkTeam `json:"team,omitempty"`
	Role       string             `json:"role,omitempty"`
	Allowed    bool               `json:"allowed"`
}

func (h *handler) networkAccessFor(ctx context.Context, userID string) (networkAccess, error) {
	st, ok := h.store.(store.NetworkTeamStore)
	if !ok {
		return networkAccess{}, errors.New("network team store unavailable")
	}
	member, err := st.GetNetworkMembership(ctx, userID)
	if err != nil {
		return networkAccess{}, err
	}

	if !member.DedicatedOnly && member.TeamID == "" {
		access := accessFromNetworkMembership(member, nil, time.Now().UTC())
		access.AccountID = userID
		return access, nil
	}
	teams, err := st.ListNetworkTeams(ctx)
	if err != nil {
		return networkAccess{}, err
	}
	access := accessFromNetworkMembership(member, teams, time.Now().UTC())
	access.AccountID = userID
	return access, nil
}

func accessFromNetworkMembership(member store.NetworkMembership, teams []store.NetworkTeam, now time.Time) networkAccess {
	access := networkAccess{Mode: "personal", Allowed: true, Configured: member.Configured}
	if !member.DedicatedOnly && member.TeamID == "" {
		return access
	}
	access.Mode, access.TeamID, access.Allowed = "dedicated", member.TeamID, false
	for _, team := range teams {
		if team.ID == member.TeamID {
			access.Team = &team
			access.Allowed = team.Active && !now.Before(team.ValidFrom) && now.Before(team.ValidUntil)
			break
		}
	}
	return access
}

// Only credential-owned groups confer a node scope. A malformed dedicated
// identity is closed, never interpreted as a legacy public credential.
func agentNetworkScope(groups []string) (string, bool) {
	team, dedicated := "", false
	for _, g := range groups {
		g = strings.TrimSpace(g)
		lower := strings.ToLower(g)
		// User administration roles are not node scopes. Reject them, and
		// misspelled reserved scope markers, rather than granting legacy public
		// access when a dedicated credential is configured incorrectly.
		if strings.HasPrefix(lower, "team-admin:") ||
			(strings.HasPrefix(lower, "team:") && !strings.HasPrefix(g, "team:")) ||
			(lower == "access:dedicated-only" && g != lower) {
			return "", false
		}
		if g == "access:dedicated-only" {
			dedicated = true
		}
		if strings.HasPrefix(g, "team:") {
			id := strings.TrimPrefix(g, "team:")
			if !networkTeamIDPattern.MatchString(id) || (team != "" && team != id) {
				return "", false
			}
			team = id
		}
	}
	return team, !dedicated || team != ""
}

func networkScopeAllows(access networkAccess, groups []string) bool {
	team, valid := agentNetworkScope(groups)
	if !valid || !access.Allowed {
		return false
	}
	if access.Mode == "dedicated" {
		return access.TeamID != "" && team == access.TeamID
	}
	return team == ""
}

type scopedAgentStatuses struct{ snapshots []agentserver.StatusSnapshot }

func (r scopedAgentStatuses) Statuses() []agentserver.StatusSnapshot { return r.snapshots }

func (h *handler) networkStatusReader(access networkAccess) agentStatusReader {
	result := scopedAgentStatuses{}
	if h.agentStatusReader != nil {
		for _, snapshot := range h.agentStatusReader.Statuses() {
			if networkScopeAllows(access, snapshot.Agent.Groups) {
				result.snapshots = append(result.snapshots, snapshot)
			}
		}
	}
	return result
}

func (h *handler) getNetworkAccess(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	access, err := h.networkAccessForUser(c.Request.Context(), user)
	if err != nil {
		respondError(c, 503, "network_access_unavailable", "network access is unavailable")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, access)
}

type networkTeamMemberPayload struct {
	BillingProfile         *store.AccountBillingProfile `json:"billingProfile"`
	QuotaState             *store.AccountQuotaState     `json:"quotaState"`
	SubscriptionValidFrom  *time.Time                   `json:"subscriptionValidFrom"`
	SubscriptionValidUntil *time.Time                   `json:"subscriptionValidUntil"`
	Groups                 []string                     `json:"groups"`

	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	QuotaGroup  string `json:"quotaGroup,omitempty"`
	QuotaLabel  string `json:"quotaLabel,omitempty"`
	AccessState string `json:"accessState"`
	Active      bool   `json:"active"`
}

type networkQuotaPlan struct {
	Value              string `json:"value"`
	PlanID             string `json:"planId"`
	Name               string `json:"name"`
	IncludedQuotaBytes int64  `json:"includedQuotaBytes"`
}
type networkTeamPayload struct {
	QuotaPlans []networkQuotaPlan `json:"quotaPlans"`

	Team        *store.NetworkTeam         `json:"team"`
	Members     []networkTeamMemberPayload `json:"members"`
	CanManage   bool                       `json:"canManage"`
	QuotaGroups []string                   `json:"quotaGroups"`
}

func networkTeamAdmin(user *store.User, teamID string) bool {
	if user == nil || strings.TrimSpace(teamID) == "" {
		return false
	}
	for _, raw := range user.Groups {
		group := strings.TrimSpace(raw)
		if group == teamAdminGroupPrefix+teamID || group == "team:"+teamID+teamAdminRoleSuffix {
			return true
		}
	}
	return false
}

func sameNetworkTeamRoles(before, after []string) bool {
	roles := func(groups []string) map[string]bool {
		result := map[string]bool{}
		for _, g := range groups {
			g = strings.TrimSpace(g)
			if strings.HasPrefix(g, teamAdminGroupPrefix) || (strings.HasPrefix(g, "team:") && strings.HasSuffix(g, teamAdminRoleSuffix)) {
				result[g] = true
			}
		}
		return result
	}
	a, b := roles(before), roles(after)
	if len(a) != len(b) {
		return false
	}
	for g := range a {
		if !b[g] {
			return false
		}
	}
	return true
}

func teamQuotaGroup(user *store.User) (string, string) {
	group := store.MonthlyQuotaGroup(user)
	switch group {
	case store.MonthlyFreeQuotaLimitGroup:
		// Keep the legacy wire value compatible with existing clients.
		return "3gb", "5 GB"
	case store.MonthlyPlusQuotaLimitGroup:
		return "20gb", "20 GB"
	case store.MonthlyUnlimitedBetaQuotaGroup:
		return "unlimited", "无限"
	default:
		return "", "未配置"
	}
}

func setTeamQuotaGroup(user *store.User, quotaGroup string) error {
	quotaGroup = strings.TrimSpace(strings.ToLower(quotaGroup))
	var selected string
	switch quotaGroup {
	case "", "none":
	case "3gb":
		selected = store.MonthlyFreeQuotaLimitGroup
	case "20gb":
		selected = store.MonthlyPlusQuotaLimitGroup
	case "unlimited":
		selected = store.MonthlyUnlimitedBetaQuotaGroup
	default:
		return errors.New("invalid quota group")
	}
	groups := make([]string, 0, len(user.Groups)+1)
	for _, group := range user.Groups {
		if group == store.MonthlyFreeQuotaLimitGroup || group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup {
			continue
		}
		groups = append(groups, group)
	}
	if selected != "" {
		groups = append(groups, selected)
	}
	user.Groups = groups
	return nil
}

func (h *handler) networkTeamForAdmin(c *gin.Context) (*store.User, networkAccess, bool) {
	actor, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return nil, networkAccess{}, false
	}
	if store.IsRootRole(actor.Role) && c.Query("teamId") != "" {
		teams, err := h.store.(store.NetworkTeamStore).ListNetworkTeams(c.Request.Context())
		if err != nil {
			respondError(c, 503, "network_access_unavailable", "network access is unavailable")
			return nil, networkAccess{}, false
		}
		for _, team := range teams {
			if team.ID == c.Query("teamId") {
				return actor, networkAccess{Mode: "dedicated", TeamID: team.ID, Team: &team, Allowed: team.Active}, true
			}
		}
		respondError(c, 404, "network_target_not_found", "team not found")
		return nil, networkAccess{}, false
	}
	access, err := h.networkAccessForUser(c.Request.Context(), actor)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "network_access_unavailable", "network access is unavailable")
		return nil, networkAccess{}, false
	}
	if access.Mode != "dedicated" || access.TeamID == "" || !networkTeamAdmin(actor, access.TeamID) {
		respondError(c, http.StatusForbidden, "team_admin_required", "team administrator access is required")
		return nil, networkAccess{}, false
	}
	return actor, access, true
}

func (h *handler) getNetworkTeam(c *gin.Context) {
	_, access, ok := h.networkTeamForAdmin(c)
	if !ok {
		return
	}
	st, ok := h.store.(store.NetworkTeamStore)
	if !ok {
		respondError(c, http.StatusServiceUnavailable, "network_access_unavailable", "network access is unavailable")
		return
	}
	memberships, err := st.ListNetworkMemberships(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "network_members_unavailable", "team members are unavailable")
		return
	}
	users, err := h.store.ListUsers(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "network_members_unavailable", "team members are unavailable")
		return
	}
	byID := make(map[string]store.User, len(users))
	for _, user := range users {
		byID[user.ID] = user
	}
	members := make([]networkTeamMemberPayload, 0)
	for _, membership := range memberships {
		if membership.TeamID != access.TeamID {
			continue
		}
		user, exists := byID[membership.UserID]
		if !exists {
			continue
		}
		quotaGroup, quotaLabel := teamQuotaGroup(&user)
		state := "active"
		quota, quotaErr := h.store.GetAccountQuotaState(c.Request.Context(), user.ID)
		if quotaErr != nil && !errors.Is(quotaErr, store.ErrUserNotFound) {
			respondError(c, 503, "quota_unavailable", "failed to load team quota")
			return
		}
		if quota != nil && quota.ProxyAccessState == "paused" {
			state = "paused"
		}
		profile, profileErr := h.store.GetAccountBillingProfile(c.Request.Context(), user.ID)
		if profileErr != nil && !errors.Is(profileErr, store.ErrUserNotFound) {
			respondError(c, 503, "billing_profile_unavailable", "failed to load team plan")
			return
		}

		role := "member"
		if networkTeamAdmin(&user, access.TeamID) {
			role = "admin"
		}
		members = append(members, networkTeamMemberPayload{BillingProfile: profile, QuotaState: quota, Groups: user.Groups, SubscriptionValidFrom: user.SubscriptionValidFrom, SubscriptionValidUntil: user.SubscriptionValidUntil, ID: user.ID, Name: user.Name, Email: user.Email, Role: role, QuotaGroup: quotaGroup, QuotaLabel: quotaLabel, AccessState: state, Active: user.Active})
	}
	team := access.Team
	if team == nil {
		respondError(c, http.StatusNotFound, "network_team_not_found", "team not found")
		return
	}
	c.Header("Cache-Control", "no-store")
	quotaPlans := make([]networkQuotaPlan, 0, 3)
	for _, entry := range []struct{ value, planID string }{{"3gb", store.BillingPlanFree}, {"20gb", store.BillingPlanPlus}, {"unlimited", store.BillingPlanUnlimitedBeta}} {
		plan, err := h.store.GetBillingPlan(c.Request.Context(), entry.planID)
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			continue
		}
		if err != nil {
			respondError(c, 503, "plan_catalog_unavailable", "failed to read billing plans")
			return
		}
		if plan != nil && plan.Active {
			quotaPlans = append(quotaPlans, networkQuotaPlan{Value: entry.value, PlanID: plan.PlanID, Name: plan.DisplayName, IncludedQuotaBytes: plan.IncludedQuotaBytes})
		}
	}
	c.JSON(http.StatusOK, networkTeamPayload{Team: team, Members: members, CanManage: true, QuotaPlans: quotaPlans, QuotaGroups: []string{"3gb", "20gb", "unlimited"}})
}

func (h *handler) addNetworkTeamMember(c *gin.Context) {
	actor, access, ok := h.networkTeamForAdmin(c)
	if !ok {
		return
	}
	var request struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Email) == "" {
		respondError(c, http.StatusBadRequest, "invalid_email", "email is required")
		return
	}
	member, err := h.store.GetUserByEmail(c.Request.Context(), strings.TrimSpace(request.Email))
	if err != nil {
		respondError(c, http.StatusNotFound, "user_not_found", "the account must register before joining a team")
		return
	}
	if member.ID == actor.ID || h.isRootAccount(member) {
		respondError(c, http.StatusBadRequest, "invalid_member", "this account cannot be added as a team member")
		return
	}
	current, err := h.networkAccessFor(c.Request.Context(), member.ID)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "network_access_unavailable", "member access is unavailable")
		return
	}
	if current.Mode == "dedicated" && current.TeamID == access.TeamID {
		respondError(c, http.StatusConflict, "member_exists", "member is already in this team")
		return
	}
	if current.Mode == "dedicated" && current.TeamID != "" {
		respondError(c, http.StatusConflict, "member_in_other_team", "member already belongs to another team")
		return
	}
	teamStore := h.store.(store.NetworkTeamStore)
	// The store validates capacity and changes the dedicated scope atomically.
	if err := teamStore.PutNetworkMembership(c.Request.Context(), store.NetworkMembership{UserID: member.ID, TeamID: access.TeamID, DedicatedOnly: true}, &store.AuditLog{Action: "network.team.member.add", ActorUUID: actor.ID, Details: map[string]any{"team_id": access.TeamID, "member_id": member.ID}}); err != nil {
		h.networkWriteError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"message": "member_added", "memberId": member.ID})
}

func (h *handler) updateNetworkTeamMember(c *gin.Context) {
	actor, access, ok := h.networkTeamForAdmin(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("userId"))
	if userID == "" || userID == actor.ID {
		respondError(c, http.StatusBadRequest, "invalid_member", "team administrators cannot modify themselves")
		return
	}
	target, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, http.StatusNotFound, "user_not_found", "member not found")
		return
	}
	member, err := h.store.(store.NetworkTeamStore).GetNetworkMembership(c.Request.Context(), userID)
	if err != nil || member.TeamID != access.TeamID {
		respondError(c, http.StatusNotFound, "member_not_found", "member is not in this team")
		return
	}
	if networkTeamAdmin(target, access.TeamID) && !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only root can modify team administrators")
		return
	}
	var request struct {
		QuotaGroup string `json:"quotaGroup"`
		Paused     *bool  `json:"paused"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid member update")
		return
	}
	if request.QuotaGroup != "" {
		if err := setTeamQuotaGroup(target, request.QuotaGroup); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_quota_group", "unsupported quota group")
			return
		}
		planID := map[string]string{"3gb": store.BillingPlanFree, "20gb": store.BillingPlanPlus, "unlimited": store.BillingPlanUnlimitedBeta}[request.QuotaGroup]
		plan, err := h.store.GetBillingPlan(c.Request.Context(), planID)
		if err != nil || plan == nil || !plan.Active {
			respondError(c, http.StatusBadRequest, "plan_unavailable", "selected quota plan is unavailable")
			return
		}
		_, tokenHash, err := generateAdminPreviewToken()
		if err != nil {
			respondError(c, http.StatusInternalServerError, "member_update_failed", "failed to prepare quota update")
			return
		}
		requestID := "team-quota:" + tokenHash
		reason := "team administrator quota update: " + access.TeamID
		change := store.AdminPlanGroupChange{UserID: target.ID, PlanID: plan.PlanID, SetEntitlement: true, Groups: target.Groups, ExpectedGroups: target.Groups, PackageName: plan.PackageName, IncludedQuotaBytes: plan.IncludedQuotaBytes, RegionMultiplier: plan.Multiplier("region"), LineMultiplier: plan.Multiplier("line"), PeakMultiplier: plan.Multiplier("peak"), OffPeakMultiplier: plan.Multiplier("offpeak"), PricingRuleVersion: "plan:" + plan.PlanID}
		original, lookupErr := h.store.GetUserByID(c.Request.Context(), target.ID)
		if lookupErr != nil {
			respondError(c, 500, "member_update_failed", "failed to read member")
			return
		}
		change.ExpectedGroups = original.Groups
		change.ExpectedValidFrom, change.ExpectedValidUntil = original.SubscriptionValidFrom, original.SubscriptionValidUntil
		_, err = h.store.CreateAdminPlanGroupPreview(c.Request.Context(), store.AdminPlanGroupPreview{TokenHash: tokenHash, ActorUUID: actor.ID, RequestID: requestID, Reason: reason, ExpiresAt: time.Now().Add(adminPlanPreviewTTL), Changes: []store.AdminPlanGroupChange{change}})
		if err == nil {
			_, _, err = h.store.ApplyAdminPlanGroupBatch(c.Request.Context(), store.AdminPlanGroupBatch{ActorUUID: actor.ID, RequestID: requestID, Reason: reason, PreviewTokenHash: tokenHash, ExpectedUserIDs: []string{target.ID}})
		}
		if err != nil {
			respondError(c, http.StatusInternalServerError, "member_update_failed", "failed to update member quota")
			return
		}
	}
	if request.Paused != nil {
		state := &store.AccountQuotaState{AccountUUID: userID, ThrottleState: "normal", SuspendState: "active", ProxyAccessState: "active"}
		if existing, stateErr := h.store.GetAccountQuotaState(c.Request.Context(), userID); stateErr == nil && existing != nil {
			state = existing
		}
		if *request.Paused {
			state.ProxyAccessState = "paused"
		} else {
			state.ProxyAccessState = "active"
		}
		if err := h.store.UpsertAccountQuotaState(c.Request.Context(), state); err != nil {
			respondError(c, http.StatusInternalServerError, "member_update_failed", "failed to update member access")
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "member_updated"})
}

func (h *handler) removeNetworkTeamMember(c *gin.Context) {
	actor, access, ok := h.networkTeamForAdmin(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("userId"))
	if userID == "" || userID == actor.ID {
		respondError(c, http.StatusBadRequest, "invalid_member", "team administrators cannot remove themselves")
		return
	}
	member, err := h.store.(store.NetworkTeamStore).GetNetworkMembership(c.Request.Context(), userID)
	if err != nil || member.TeamID != access.TeamID {
		respondError(c, http.StatusNotFound, "member_not_found", "member is not in this team")
		return
	}
	target, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, 404, "member_not_found", "member not found")
		return
	}
	if networkTeamAdmin(target, access.TeamID) && !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only root can remove team administrators")
		return
	}
	if err := h.store.(store.NetworkTeamStore).PutNetworkMembership(c.Request.Context(), store.NetworkMembership{UserID: userID, DedicatedOnly: true}, &store.AuditLog{Action: "network.team.member.remove", ActorUUID: actor.ID, Details: map[string]any{"team_id": access.TeamID, "member_id": userID}}); err != nil {
		h.networkWriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "member_removed"})
}

func (h *handler) adminGetNetworkMembership(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite); !ok {
		return
	}
	user, err := h.store.GetUserByID(c.Request.Context(), c.Param("userId"))
	if err != nil {
		h.networkWriteError(c, err)
		return
	}
	access, err := h.networkAccessFor(c.Request.Context(), c.Param("userId"))
	if err != nil {
		h.networkWriteError(c, err)
		return
	}
	access.Role = "member"
	if networkTeamAdmin(user, access.TeamID) {
		access.Role = "admin"
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, access)
}

func (h *handler) adminListNetworkTeams(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsRead); !ok {
		return
	}
	teams, err := h.store.(store.NetworkTeamStore).ListNetworkTeams(c.Request.Context())
	if err != nil {
		respondError(c, 503, "network_access_unavailable", "network access is unavailable")
		return
	}
	c.JSON(200, gin.H{"teams": teams})
}

func (h *handler) adminPutNetworkTeam(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only root can configure teams")
		return
	}
	var req struct {
		store.NetworkTeam
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, 400, "invalid_request", "invalid team")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	req.ID = c.Param("teamId")
	if !networkTeamIDPattern.MatchString(req.ID) || strings.TrimSpace(req.Name) == "" || req.MemberLimit < 1 || req.MemberLimit > 10000 || req.ValidFrom.IsZero() || !req.ValidUntil.After(req.ValidFrom) {
		respondError(c, 400, "invalid_team", "team requires a name, member limit and ordered validity dates")
		return
	}
	err := h.store.(store.NetworkTeamStore).PutNetworkTeam(c.Request.Context(), req.NetworkTeam, &store.AuditLog{Action: "network.team.upsert", ActorUUID: actor.ID, Details: map[string]any{"reason": reason, "team": req.NetworkTeam}})
	if err != nil {
		h.networkWriteError(c, err)
		return
	}
	c.JSON(200, req.NetworkTeam)
}

func (h *handler) adminPutNetworkMembership(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite)
	if !ok {
		return
	}
	if !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only root can set team membership and administrator roles")
		return
	}
	var req struct {
		Mode   string `json:"mode"`
		TeamID string `json:"teamId"`
		Role   string `json:"role"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, 400, "invalid_request", "invalid membership")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	if (req.Mode != "personal" && req.Mode != "dedicated") || (req.Mode == "personal" && req.TeamID != "") || (req.TeamID != "" && !networkTeamIDPattern.MatchString(req.TeamID)) {
		respondError(c, 400, "invalid_network_access", "mode must be personal or dedicated, with at most one team")
		return
	}
	req.Role = strings.TrimSpace(strings.ToLower(req.Role))
	if req.Role != "" && req.Role != "member" && req.Role != "admin" {
		respondError(c, 400, "invalid_network_role", "role must be member or admin")
		return
	}
	if req.Role == "admin" && (req.Mode != "dedicated" || req.TeamID == "") {
		respondError(c, 400, "invalid_network_role", "team administrators must belong to a dedicated team")
		return
	}
	if req.Role == "admin" && !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only a root administrator can appoint team administrators")
		return
	}
	user, err := h.store.GetUserByID(c.Request.Context(), c.Param("userId"))
	if err != nil {
		h.networkWriteError(c, err)
		return
	}
	if h.isRootAccount(user) || h.isReadOnlyAccount(user) {
		respondError(c, 403, "root_protected", "root network access cannot be modified")
		return
	}
	member := store.NetworkMembership{UserID: user.ID, TeamID: req.TeamID, DedicatedOnly: req.Mode == "dedicated", Role: req.Role}
	err = h.store.(store.NetworkTeamStore).PutNetworkMembership(c.Request.Context(), member, &store.AuditLog{Action: "network.membership.set", ActorUUID: actor.ID, Details: map[string]any{"target_uuid": user.ID, "reason": reason, "membership": member, "platform_root": true}})
	if err != nil {
		h.networkWriteError(c, err)
		return
	}
	c.JSON(200, member)
}

func (h *handler) networkWriteError(c *gin.Context, err error) {
	status, code := http.StatusInternalServerError, "network_access_write_failed"
	if errors.Is(err, store.ErrNetworkWithdrawalRequired) {
		status, code = 409, "network_withdrawal_required"
	}
	if errors.Is(err, store.ErrNetworkTeamFull) {
		status, code = 409, "team_member_limit"
	}
	if errors.Is(err, store.ErrNetworkTeamNotFound) || errors.Is(err, store.ErrUserNotFound) {
		status, code = 404, "network_target_not_found"
	}
	respondError(c, status, code, code)
}

// Dedicated credentials cannot alias another ID. Legacy shared public
// credentials may still register public aliases, but cannot overwrite a team ID.
// The node's declared class selects a public pool only when its authenticated
// deployment credential can manage public pools. Team and restricted credentials
// remain bound to their enrolled scope; pool names never authorize a scope.
func (h *handler) resolveScopedAgent(c *gin.Context, credential agentserver.Identity, requested string) (agentserver.Identity, bool) {
	identity, ok := h.resolveAgentIdentity(c, credential, requested)
	if !ok {
		return identity, false
	}
	class := strings.TrimSpace(c.GetHeader("X-Agent-Resource-Class"))
	teamID := strings.TrimSpace(c.GetHeader("X-Agent-Team-ID"))
	if class == "" && teamID == "" {
		return identity, true
	}
	if class != agentResourceShared && class != agentResourceFree && class != agentResourcePlus && class != agentResourceTeam {
		respondError(c, 400, "agent_scope_invalid", "resource class must be shared, free, plus or team")
		return identity, false
	}
	if class != agentResourceTeam && teamID != "" || class == agentResourceTeam && teamID == "" {
		respondError(c, 400, "agent_scope_invalid", "team ID is required only for team nodes")
		return identity, false
	}
	scope, valid := agentResourceScope(identity.Groups)
	if !valid {
		respondError(c, 403, "agent_scope_invalid", "invalid registered agent scope")
		return identity, false
	}
	credentialScope, valid := agentResourceScope(credential.Groups)
	if !valid {
		respondError(c, 403, "agent_scope_invalid", "invalid credential scope")
		return identity, false
	}
	managePublic := false
	for _, group := range credential.Groups {
		if group == "agent:configure-public" || (group == "internal" && credential.ID == "*" && credentialScope.ResourceClass == agentResourceShared) {
			managePublic = true
		}
	}
	managePublic = managePublic && (credential.ID == "*" || credential.ID == identity.ID)
	if managePublic && credentialScope.ResourceClass != agentResourceTeam && scope.ResourceClass != agentResourceTeam && class != agentResourceTeam {
		groups := make([]string, 0, len(identity.Groups)+1)
		for _, group := range identity.Groups {
			if !strings.HasPrefix(strings.TrimSpace(group), agentResourceGroupPrefix) {
				groups = append(groups, group)
			}
		}
		identity.Groups = append(groups, agentResourceGroupPrefix+class)
		return identity, true
	}
	if scope.ResourceClass != class || scope.TeamID != teamID || (!managePublic && scope != credentialScope) {
		respondError(c, 403, "agent_scope_mismatch", "requested class requires matching enrolled scope or public pool management permission")
		return identity, false
	}
	return identity, true
}

func (h *handler) resolveAgentIdentity(c *gin.Context, credential agentserver.Identity, requested string) (agentserver.Identity, bool) {
	team, valid := agentNetworkScope(credential.Groups)
	if !valid {
		respondError(c, http.StatusForbidden, "agent_scope_invalid", "agent credential has an invalid network scope")
		return credential, false
	}
	if requested == "" {
		requested = strings.TrimSpace(c.GetHeader(agentIDHeader))
	}
	if requested == "" {
		requested = strings.TrimSpace(c.Query("agentId"))
	}
	if requested == "" || requested == credential.ID {
		return credential, true
	}
	if team != "" {
		respondError(c, 403, "agent_scope_mismatch", "agent identity is bound to its credential")
		return credential, false
	}
	inspector, canInspect := h.agentRegistry.(interface{ Agents() []agentserver.Identity })
	if !canInspect {
		respondError(c, 403, "agent_scope_mismatch", "agent alias requires a credential registry")
		return credential, false
	}
	for _, identity := range inspector.Agents() {
		if identity.ID == requested {
			target, ok := agentNetworkScope(identity.Groups)
			if !ok || target != "" {
				respondError(c, 403, "agent_scope_mismatch", "agent identity is bound to its credential")
				return credential, false
			}
		}
	}
	return h.agentRegistry.RegisterAgent(requested, credential.Groups), true
}

func (h *handler) networkAccessForUser(ctx context.Context, user *store.User) (networkAccess, error) {
	access, err := h.networkAccessFor(ctx, user.ID)
	if err != nil {
		return access, err
	}
	if !user.Active || (!user.EmailVerified && !strings.EqualFold(user.Email, sandboxUserEmail)) {
		access.Allowed = false
		return access, nil
	}
	blocked, err := h.store.ListProxyBlockedAccountUUIDs(ctx)
	if err != nil {
		return access, err
	}
	if blocked[user.ID] {
		access.Allowed = false
	}
	return access, nil
}

func (h *handler) reservedDedicatedHost(host string) bool {
	if h.agentStatusReader == nil {
		return false
	}
	for _, snapshot := range h.agentStatusReader.Statuses() {
		team, valid := agentNetworkScope(snapshot.Agent.Groups)
		if (!valid || team != "") && (normalizeHost(snapshot.Agent.ID) == host || regionalEntryHost(snapshot.Report.Xray.EntryPoint) == host) {
			return true
		}
	}
	return false
}
