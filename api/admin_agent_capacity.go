package api

import (
	"net/http"
	"regexp"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

var (
	trafficInterfacePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)
	trafficBootIDPattern    = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
)

func proxyInventoryAgent(snapshot agentserver.StatusSnapshot) bool {
	role := snapshot.Report.Role
	class, _ := reportedResourceClass(snapshot)
	return snapshot.Agent.ID != "" && snapshot.Agent.ID != "*" && (role == "" || role == "agent-proxy") &&
		class != "" && class != agentResourceTeam && !managedTeam(snapshot.Agent.Groups)
}

func (h *handler) recordAgentTraffic(c *gin.Context, credential, identity agentserver.Identity, report agentproto.StatusReport) {
	sample := report.NodeTraffic
	if sample == nil || identity.ID == "" || identity.ID == "*" || (credential.ID != identity.ID && !deploymentCredential(credential)) ||
		(report.Role != "" && report.Role != "agent-proxy") {
		return
	}
	now := time.Now().UTC()
	if !trafficInterfacePattern.MatchString(sample.Interface) || !trafficBootIDPattern.MatchString(sample.BootID) ||
		sample.RXBytes == nil || sample.TXBytes == nil || *sample.RXBytes < 0 || *sample.TXBytes < 0 ||
		sample.SampledAt.Before(now.Add(-24*time.Hour)) || sample.SampledAt.After(now.Add(30*time.Second)) {
		return
	}
	if st, ok := h.store.(store.AgentNodeCapacityStore); ok {
		if err := st.RecordAgentNodeTraffic(c.Request.Context(), identity.ID, *sample); err != nil {
			// Preserve existing heartbeat health even when capacity storage is unavailable.
			c.Error(err)
		}
	}
}

func (h *handler) readAgentCapacity(c *gin.Context, id string, now time.Time) *store.AgentNodeCapacity {
	if st, ok := h.store.(store.AgentNodeCapacityStore); ok {
		capacity, err := st.GetAgentNodeCapacity(c.Request.Context(), id, now)
		if err != nil {
			c.Error(err)
		}
		return capacity
	}
	return nil
}

func (h *handler) adminPutAgentCapacity(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !store.IsAdminRole(actor.Role) {
		respondError(c, http.StatusForbidden, "forbidden", "agent capacity requires an administrator")
		return
	}
	var req *store.AgentNodeCapacityLimits
	if c.ShouldBindJSON(&req) != nil || req == nil ||
		(req.MonthlyLimitBytes != nil && (*req.MonthlyLimitBytes <= 0 || *req.MonthlyLimitBytes > 1<<53-1)) ||
		(req.BandwidthLimitMbps != nil && *req.BandwidthLimitMbps <= 0) {
		respondError(c, http.StatusBadRequest, "invalid_agent_capacity", "limits must be positive integers or null")
		return
	}
	id := c.Param("agentId")
	found := false
	for _, snapshot := range h.enrollmentStatusReader().Statuses() {
		if snapshot.Agent.ID == id && proxyInventoryAgent(snapshot) {
			found = true
			break
		}
	}
	if !found && store.IsRootRole(actor.Role) {
		if teams, ok := h.store.(store.TeamAgentEnrollmentStore); ok {
			rows, err := teams.ListTeamAgents(c.Request.Context())
			if err != nil {
				respondError(c, http.StatusServiceUnavailable, "agent_capacity_unavailable", "team enrollment store is unavailable")
				return
			}
			for _, row := range rows {
				if row.AgentID == id {
					found = true
					break
				}
			}
		}
	}
	if !found {
		respondError(c, http.StatusNotFound, "agent_not_found", "node is not in the editable agent inventory")
		return
	}
	st, ok := h.store.(store.AgentNodeCapacityStore)
	if !ok {
		respondError(c, http.StatusServiceUnavailable, "agent_capacity_unavailable", "node capacity store is unavailable")
		return
	}
	audit := &store.AuditLog{
		Action: "network.agent.capacity", ActorUUID: actor.ID,
		Details: map[string]any{"agentId": id, "monthlyLimitBytes": req.MonthlyLimitBytes, "bandwidthLimitMbps": req.BandwidthLimitMbps},
	}
	if err := st.SetAgentNodeCapacity(c.Request.Context(), id, *req, audit); err != nil {
		c.Error(err)
		respondError(c, http.StatusServiceUnavailable, "agent_capacity_unavailable", "node limits could not be saved")
		return
	}
	c.Status(http.StatusNoContent)
}
