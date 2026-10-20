package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentserver"
	"account/internal/store"
)

type agentStatusReader interface {
	Statuses() []agentserver.StatusSnapshot
}

type agentStatusEntry struct {
	ID           string           `json:"id"`
	Name         string           `json:"name,omitempty"`
	Groups       []string         `json:"groups,omitempty"`
	Healthy      bool             `json:"healthy"`
	Message      string           `json:"message,omitempty"`
	Users        int              `json:"users"`
	SyncRevision string           `json:"syncRevision,omitempty"`
	UpdatedAt    time.Time        `json:"updatedAt"`
	Xray         agentXraySummary `json:"xray"`
}

type agentXraySummary struct {
	Running  bool       `json:"running"`
	Clients  int        `json:"clients"`
	LastSync *time.Time `json:"lastSync,omitempty"`
}

func (h *handler) adminAgentStatus(c *gin.Context) {
	if h.agentStatusReader == nil {
		respondError(c, http.StatusServiceUnavailable, "agent_status_unavailable", "agent registry is not configured")
		return
	}

	actor, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus)
	if !ok {
		return
	}

	snapshots := h.agentStatusReader.Statuses()
	if !store.IsRootRole(actor.Role) {
		access, err := h.networkAccessForUser(c.Request.Context(), actor)
		if err != nil {
			respondError(c, 503, "network_access_unavailable", "network access is unavailable")
			return
		}
		packageName, err := h.billingPackageName(c.Request.Context(), actor)
		if err != nil {
			respondError(c, 503, "billing_profile_unavailable", "failed to evaluate subscription resource class")
			return
		}
		visible := make([]agentserver.StatusSnapshot, 0)
		for _, snapshot := range h.networkStatusReader(access).Statuses() {
			scope, valid := agentResourceScope(snapshot.Agent.Groups)
			if valid && agentResourceScopeAllowsUser(scope, access, actor, packageName) {
				visible = append(visible, snapshot)
			}
		}
		snapshots = visible
	}
	entries := make([]agentStatusEntry, 0, len(snapshots))
	for _, snapshot := range snapshots {
		entry := agentStatusEntry{
			ID:           snapshot.Agent.ID,
			Name:         snapshot.Agent.Name,
			Groups:       append([]string(nil), snapshot.Agent.Groups...),
			Healthy:      snapshot.Report.Healthy,
			Message:      snapshot.Report.Message,
			Users:        snapshot.Report.Users,
			SyncRevision: snapshot.Report.SyncRevision,
			UpdatedAt:    snapshot.UpdatedAt,
			Xray: agentXraySummary{
				Running: snapshot.Report.Xray.Running,
				Clients: snapshot.Report.Xray.Clients,
			},
		}
		if snapshot.Report.Xray.LastSync != nil {
			last := *snapshot.Report.Xray.LastSync
			entry.Xray.LastSync = &last
		}
		entries = append(entries, entry)
	}

	c.JSON(http.StatusOK, gin.H{"agents": entries})
}
