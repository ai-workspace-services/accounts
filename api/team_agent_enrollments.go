package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"account/internal/xrayconfig"
	"github.com/gin-gonic/gin"
)

const managedTeamMarker = "enrollment:team"

func managedTeam(groups []string) bool {
	for _, g := range groups {
		if g == managedTeamMarker {
			return true
		}
	}
	return false
}
func enrollmentGroups(e store.TeamAgentEnrollment) []string {
	if e.Status == "approved" {
		return []string{managedTeamMarker, "team:" + e.AssignedTeamID}
	}
	return []string{managedTeamMarker, "access:dedicated-only"}
}
func enrollmentRevision(e store.TeamAgentEnrollment) string {
	sum := sha256.Sum256([]byte(e.AgentID + "\x00" + e.Status + "\x00" + e.AssignedTeamID + "\x00" + e.UpdatedAt.UTC().Format(time.RFC3339Nano)))
	return "enrollment-" + hex.EncodeToString(sum[:])
}
func deploymentCredential(identity agentserver.Identity) bool {
	// This enrollment path is available only to the existing shared deployment
	// credential. Restricted and team-owned credentials cannot claim other IDs.
	scope, valid := agentResourceScope(identity.Groups)
	if !valid || scope.ResourceClass != agentResourceShared || identity.ID != "*" {
		return false
	}
	for _, g := range identity.Groups {
		if g == "internal" || g == "agent:configure-public" {
			return true
		}
	}
	return false
}
func (h *handler) resolveManagedTeam(c *gin.Context, credential agentserver.Identity, requested string) (agentserver.Identity, bool, bool) {
	class := strings.TrimSpace(c.GetHeader("X-Agent-Resource-Class"))
	if requested == "" {
		requested = strings.TrimSpace(c.GetHeader(agentIDHeader))
	}
	if requested == "" {
		requested = strings.TrimSpace(c.Query("agentId"))
	}
	if !deploymentCredential(credential) {
		return credential, false, false
	}
	existingManaged := false
	if inspector, ok := h.agentRegistry.(interface{ Agents() []agentserver.Identity }); ok {
		for _, a := range inspector.Agents() {
			if a.ID == requested && managedTeam(a.Groups) {
				existingManaged = true
			}
		}
	}
	if class != "team" && !existingManaged {
		if st, ok := h.store.(store.TeamAgentEnrollmentStore); ok {
			rows, err := st.ListTeamAgents(c.Request.Context())
			if err != nil {
				respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
				return credential, false, true
			}
			if err == nil {
				for _, e := range rows {
					if e.AgentID == requested {
						existingManaged = true
						break
					}
				}
			}
		}
	}
	if class != "team" && !existingManaged {
		return credential, false, false
	}
	if class != "team" {
		respondError(c, 403, "agent_scope_mismatch", "registered team node cannot switch to a public scope")
		return credential, false, true
	}
	suggested := strings.TrimSpace(c.GetHeader("X-Agent-Team-ID"))
	if requested == "" || requested == "*" || len(requested) > 253 || strings.ContainsAny(requested, " /\t\r\n") || (suggested != "" && !networkTeamIDPattern.MatchString(suggested)) {
		respondError(c, 400, "agent_scope_invalid", "team enrollment requires a valid node ID and optional suggested team ID")
		return credential, false, true
	}
	// A legacy credential-owned team identity cannot be taken over by the shared token.
	if inspector, ok := h.agentRegistry.(interface{ Agents() []agentserver.Identity }); ok {
		for _, a := range inspector.Agents() {
			if a.ID == requested && !managedTeam(a.Groups) {
				team, valid := agentNetworkScope(a.Groups)
				if !valid || team != "" {
					respondError(c, 403, "agent_scope_mismatch", "agent identity is bound to its credential")
					return credential, false, true
				}
			}
		}
	}
	st, ok := h.store.(store.TeamAgentEnrollmentStore)
	if !ok {
		respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
		return credential, false, true
	}
	e, err := st.RegisterTeamAgent(c.Request.Context(), requested, suggested)
	if err != nil {
		respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
		return credential, false, true
	}

	identity := h.agentRegistry.RegisterAgent(requested, enrollmentGroups(e))
	identity.Groups = enrollmentGroups(e)
	if setter, ok := h.agentRegistry.(interface{ SetAgentGroups(string, []string) }); ok {
		setter.SetAgentGroups(identity.ID, identity.Groups)
	}
	c.Set("teamAgentEnrollment", e)
	return identity, true, true
}
func teamEnrollment(c *gin.Context) (store.TeamAgentEnrollment, bool) {
	value, ok := c.Get("teamAgentEnrollment")
	if !ok {
		return store.TeamAgentEnrollment{}, false
	}
	e, ok := value.(store.TeamAgentEnrollment)
	return e, ok
}
func pendingTeamResponse(c *gin.Context) bool {
	e, ok := teamEnrollment(c)
	if !ok || e.Status == "approved" {
		return false
	}
	c.JSON(200, agentproto.ClientListResponse{EnrollmentStatus: e.Status, Scope: &agentproto.UserScope{ResourceClass: "team"}, Clients: []xrayconfig.Client{}, Revision: enrollmentRevision(e), GeneratedAt: time.Now().UTC()})
	return true
}
func (h *handler) adminListTeamAgents(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus)
	if !ok {
		return
	}
	teamID := ""
	root := store.IsRootRole(actor.Role)
	if !root {
		access, err := h.networkAccessForUser(c.Request.Context(), actor)
		if err != nil {
			respondError(c, 503, "network_access_unavailable", "network access is unavailable")
			return
		}
		if access.Allowed && access.Mode == "dedicated" {
			teamID = access.TeamID
		}
	}
	st, ok := h.store.(store.TeamAgentEnrollmentStore)
	if !ok {
		respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
		return
	}
	rows, err := st.ListTeamAgents(c.Request.Context())
	if err != nil {
		respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
		return
	}
	type row struct {
		store.TeamAgentEnrollment
		Online        bool      `json:"online"`
		LastHeartbeat time.Time `json:"lastHeartbeat"`
		Region        string    `json:"region"`
		EntryPoint    string    `json:"entryPoint"`
	}
	out := make([]row, 0, len(rows))
	for _, e := range rows {
		// Suggested ownership is untrusted; only an assigned team grants visibility.
		if !root && (teamID == "" || e.AssignedTeamID != teamID) {
			continue
		}
		if !root {
			e.SuggestedTeamID = ""
		}
		v := row{TeamAgentEnrollment: e}
		if h.agentStatusReader != nil {
			for _, s := range h.agentStatusReader.Statuses() {
				if s.Agent.ID == e.AgentID {
					v.LastHeartbeat = s.UpdatedAt
					v.Online = !s.UpdatedAt.IsZero() && time.Since(s.UpdatedAt) < regionalHeartbeatTTL
					v.Region = s.Report.Xray.Region
					v.EntryPoint = s.Report.Xray.EntryPoint
				}
			}
		}
		out = append(out, v)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"agents": out})
}
func (h *handler) adminDecideTeamAgent(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only root can assign team nodes")
		return
	}
	var req struct {
		AgentID   string    `json:"agentId"`
		Status    string    `json:"status"`
		TeamID    string    `json:"teamId"`
		Reason    string    `json:"reason"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "invalid decision")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	if req.Status != "approved" && req.Status != "rejected" && req.Status != "revoked" {
		respondError(c, 400, "invalid_request", "invalid approval status")
		return
	}
	st, ok := h.store.(store.TeamAgentEnrollmentStore)
	if !ok {
		respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
		return
	}
	rows, err := st.ListTeamAgents(c.Request.Context())
	if err != nil {
		respondError(c, 503, "agent_enrollment_unavailable", "team enrollment store unavailable")
		return
	}
	var old store.TeamAgentEnrollment
	for _, e := range rows {
		if e.AgentID == req.AgentID {
			old = e
			break
		}
	}
	if old.AgentID == "" {
		respondError(c, 404, "agent_not_found", "team node not found")
		return
	}
	if !old.UpdatedAt.Equal(req.UpdatedAt) {
		respondError(c, 409, "agent_enrollment_conflict", "refresh enrollment before deciding")
		return
	}
	if req.Status == "approved" {
		if !networkTeamIDPattern.MatchString(req.TeamID) {
			respondError(c, 400, "invalid_team", "select a team")
			return
		}
		teams, err := h.store.(store.NetworkTeamStore).ListNetworkTeams(c.Request.Context())
		if err != nil {
			respondError(c, 503, "network_access_unavailable", "teams unavailable")
			return
		}
		found := false
		for _, t := range teams {
			if t.ID == req.TeamID {
				found = true
			}
		}
		if !found {
			respondError(c, 404, "network_target_not_found", "team not found")
			return
		}
		if old.Status == "approved" && old.AssignedTeamID != req.TeamID {
			respondError(c, 409, "network_withdrawal_required", "revoke old assignment before changing team")
			return
		}
		if old.Status != "approved" && old.AssignedTeamID != "" {
			cleared := false
			if h.agentStatusReader != nil {
				for _, s := range h.agentStatusReader.Statuses() {
					if s.Agent.ID == old.AgentID && s.Report.SyncRevision == enrollmentRevision(old) && s.Report.Users == 0 && s.Report.Xray.Clients == 0 && s.Report.Healthy && s.Report.Xray.Running && s.Report.Xray.LastSync != nil && s.Report.Xray.LastSync.After(old.UpdatedAt) && s.UpdatedAt.After(old.UpdatedAt) {
						cleared = true
					}
				}
			}
			if !cleared {
				respondError(c, 409, "network_withdrawal_required", "wait for node to apply empty authorization before approval")
				return
			}
		}
	}
	next := old
	next.Status = req.Status
	next.DecidedBy = actor.ID
	next.Reason = reason
	if req.Status == "approved" {
		next.AssignedTeamID = req.TeamID
	}
	audit := &store.AuditLog{Action: "network.agent.assignment", ActorUUID: actor.ID, Details: map[string]any{"reason": reason, "before": old, "after": next}}
	if err := st.DecideTeamAgent(c.Request.Context(), next, old.UpdatedAt, audit); err != nil {
		if errors.Is(err, store.ErrTeamAgentConflict) {
			respondError(c, 409, "agent_enrollment_conflict", "decision failed; refresh enrollment")
		} else {
			respondError(c, 503, "agent_enrollment_unavailable", "decision could not be saved")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// Overlay persisted administrator decisions on telemetry at every discovery read.
// A previous heartbeat must not keep a revoked or reassigned node available.
func (h *handler) enrollmentStatusReader() agentStatusReader {
	if h.agentStatusReader == nil {
		return scopedAgentStatuses{}
	}
	snapshots := h.agentStatusReader.Statuses()
	st, ok := h.store.(store.TeamAgentEnrollmentStore)
	entries := map[string]store.TeamAgentEnrollment{}
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		rows, err := st.ListTeamAgents(ctx)
		if err != nil {
			return scopedAgentStatuses{}
		}
		if err == nil {
			for _, e := range rows {
				entries[e.AgentID] = e
			}
		}
	}
	for i := range snapshots {
		e, found := entries[snapshots[i].Agent.ID]
		if found {
			snapshots[i].Agent.Groups = enrollmentGroups(e)
		}
		if found || managedTeam(snapshots[i].Agent.Groups) {
			ready := found && e.Status == "approved" && snapshots[i].Report.Healthy && snapshots[i].Report.Xray.Running && snapshots[i].Report.Xray.LastSync != nil && snapshots[i].Report.Xray.LastSync.After(e.UpdatedAt) && strings.HasPrefix(snapshots[i].Report.SyncRevision, enrollmentRevision(e)+"-")
			if !ready {
				closed := false
				snapshots[i].Report.Xray.OpenToUsers = &closed
				snapshots[i].Agent.Groups = []string{managedTeamMarker, "access:dedicated-only"}
			}
		}
	}
	return scopedAgentStatuses{snapshots: snapshots}
}
