package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

var networkTeamIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type networkAccess struct {
	AccountID  string             `json:"accountId,omitempty"`
	Configured bool               `json:"-"`
	Mode       string             `json:"mode"`
	TeamID     string             `json:"teamId,omitempty"`
	Team       *store.NetworkTeam `json:"team,omitempty"`
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

func (h *handler) adminGetNetworkMembership(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite); !ok {
		return
	}
	if _, err := h.store.GetUserByID(c.Request.Context(), c.Param("userId")); err != nil {
		h.networkWriteError(c, err)
		return
	}
	access, err := h.networkAccessFor(c.Request.Context(), c.Param("userId"))
	if err != nil {
		h.networkWriteError(c, err)
		return
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
	var req struct {
		Mode   string `json:"mode"`
		TeamID string `json:"teamId"`
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
	user, err := h.store.GetUserByID(c.Request.Context(), c.Param("userId"))
	if err != nil {
		h.networkWriteError(c, err)
		return
	}
	if h.isRootAccount(user) || h.isReadOnlyAccount(user) {
		respondError(c, 403, "root_protected", "root network access cannot be modified")
		return
	}
	member := store.NetworkMembership{UserID: user.ID, TeamID: req.TeamID, DedicatedOnly: req.Mode == "dedicated"}
	err = h.store.(store.NetworkTeamStore).PutNetworkMembership(c.Request.Context(), member, &store.AuditLog{Action: "network.membership.set", ActorUUID: actor.ID, Details: map[string]any{"target_uuid": user.ID, "reason": reason, "membership": member}})
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
func (h *handler) resolveScopedAgent(c *gin.Context, credential agentserver.Identity, requested string) (agentserver.Identity, bool) {
	if requested == "" {
		requested = strings.TrimSpace(c.GetHeader(agentIDHeader))
	}
	if requested == "" {
		requested = strings.TrimSpace(c.Query("agentId"))
	}
	if requested == "" || requested == credential.ID {
		return credential, true
	}
	team, valid := agentNetworkScope(credential.Groups)
	if !valid || team != "" {
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
