package api

import (
	"net/http"
	"time"

	"account/internal/store"

	"github.com/gin-gonic/gin"
)

type agentInventoryEntry struct {
	ID            string     `json:"id"`
	NodeID        string     `json:"nodeId,omitempty"`
	Name          string     `json:"name,omitempty"`
	ResourceClass string     `json:"resourceClass"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	LastSync      *time.Time `json:"lastSync,omitempty"`
}

func (h *handler) adminAgentInventory(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus)
	if !ok {
		return
	}
	if !store.IsAdminRole(actor.Role) && !store.IsOperatorRole(actor.Role) {
		respondError(c, http.StatusForbidden, "forbidden", "agent inventory requires an administrative role")
		return
	}
	if h.agentStatusReader == nil {
		respondError(c, http.StatusServiceUnavailable, "agent_status_unavailable", "agent registry is not configured")
		return
	}

	// This is the proxy inventory, independent of the operator's personal plan.
	// Team enrollments and dedicated nodes remain on their scoped endpoint.
	snapshots := h.enrollmentStatusReader().Statuses()
	entries := make([]agentInventoryEntry, 0, len(snapshots))
	now := time.Now().UTC()
	for _, snapshot := range snapshots {
		role := snapshot.Report.Role
		if snapshot.Agent.ID == "" || snapshot.Agent.ID == "*" || (role != "" && role != "agent-proxy") {
			continue
		}
		class, _ := reportedResourceClass(snapshot)
		if class == "" || class == agentResourceTeam || managedTeam(snapshot.Agent.Groups) {
			continue
		}
		entry := agentInventoryEntry{
			ID:            snapshot.Agent.ID,
			NodeID:        snapshot.Agent.NodeID,
			Name:          snapshot.Agent.Name,
			ResourceClass: class,
			Status:        "unknown",
			LastSync:      snapshot.Report.Xray.LastSync,
		}
		if !snapshot.UpdatedAt.IsZero() {
			entry.LastHeartbeat = &snapshot.UpdatedAt
			switch {
			case snapshot.UpdatedAt.After(now) || now.Sub(snapshot.UpdatedAt) >= regionalHeartbeatTTL:
				entry.Status = "offline"
			case !snapshot.Report.Healthy || !snapshot.Report.Xray.Running:
				entry.Status = "unhealthy"
			default:
				entry.Status = "healthy"
			}
		}
		entries = append(entries, entry)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"agents": entries})
}
