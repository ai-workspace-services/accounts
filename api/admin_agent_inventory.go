package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"

	"github.com/gin-gonic/gin"
)

type agentInventoryEntry struct {
	ID            string     `json:"id"`
	NodeID        string     `json:"nodeId,omitempty"`
	Name          string     `json:"name,omitempty"`
	ResourceClass string     `json:"resourceClass"`
	Status        string     `json:"status"`
	ClientCount   *int       `json:"clientCount,omitempty"`
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	LastSync      *time.Time `json:"lastSync,omitempty"`
}

func isGatewayAgent(snapshot agentserver.StatusSnapshot) bool {
	if strings.EqualFold(strings.TrimSpace(snapshot.Report.Role), "gateway") {
		return true
	}
	for _, candidate := range []string{snapshot.Agent.ID, snapshot.Agent.NodeID, snapshot.Agent.Name} {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if strings.Contains(lower, "-gateway-") || strings.HasSuffix(lower, "-gateway") || strings.HasPrefix(lower, "gateway-") {
			return true
		}
	}
	return false
}

func mergeAgentsFromStore(ctx context.Context, snapshots []agentserver.StatusSnapshot, st store.Store) []agentserver.StatusSnapshot {
	if st == nil {
		return snapshots
	}
	dbAgents, err := st.ListAgents(ctx)
	if err != nil || len(dbAgents) == 0 {
		return snapshots
	}

	byID := make(map[string]int, len(snapshots))
	merged := make([]agentserver.StatusSnapshot, len(snapshots))
	copy(merged, snapshots)
	for i, snap := range merged {
		byID[snap.Agent.ID] = i
	}

	for _, dbAgent := range dbAgents {
		if dbAgent == nil || dbAgent.ID == "" {
			continue
		}
		idx, exists := byID[dbAgent.ID]
		if !exists {
			snap := agentserver.StatusSnapshot{
				Agent: agentserver.Identity{
					ID:     dbAgent.ID,
					Name:   dbAgent.Name,
					Groups: dbAgent.Groups,
				},
				Report: agentproto.StatusReport{
					AgentID:      dbAgent.ID,
					Role:         "agent-proxy",
					Healthy:      dbAgent.Healthy,
					Users:        dbAgent.ClientsCount,
					SyncRevision: dbAgent.SyncRevision,
					Xray: agentproto.XrayStatus{
						Running: dbAgent.Healthy,
						Clients: dbAgent.ClientsCount,
					},
				},
			}
			if dbAgent.LastHeartbeat != nil {
				snap.UpdatedAt = dbAgent.LastHeartbeat.UTC()
			}
			byID[dbAgent.ID] = len(merged)
			merged = append(merged, snap)
			continue
		}

		snap := merged[idx]
		if dbAgent.LastHeartbeat != nil && (snap.UpdatedAt.IsZero() || dbAgent.LastHeartbeat.UTC().After(snap.UpdatedAt.UTC())) {
			snap.UpdatedAt = dbAgent.LastHeartbeat.UTC()
			snap.Report.Healthy = dbAgent.Healthy
			snap.Report.Xray.Running = dbAgent.Healthy
			snap.Report.Users = dbAgent.ClientsCount
			snap.Report.Xray.Clients = dbAgent.ClientsCount
			snap.Report.SyncRevision = dbAgent.SyncRevision
			if len(dbAgent.Groups) > 0 {
				snap.Agent.Groups = dbAgent.Groups
			}
			merged[idx] = snap
		}
	}
	return merged
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
	if h.agentStatusReader == nil && h.store == nil {
		respondError(c, http.StatusServiceUnavailable, "agent_status_unavailable", "agent registry is not configured")
		return
	}

	// This is the proxy inventory, independent of the operator's personal plan.
	// Team enrollments and dedicated nodes remain on their scoped endpoint.
	var snapshots []agentserver.StatusSnapshot
	if h.agentStatusReader != nil {
		snapshots = h.enrollmentStatusReader().Statuses()
	}
	snapshots = mergeAgentsFromStore(c.Request.Context(), snapshots, h.store)
	entries := make([]agentInventoryEntry, 0, len(snapshots))
	now := time.Now().UTC()
	for _, snapshot := range snapshots {
		role := snapshot.Report.Role
		if snapshot.Agent.ID == "" || snapshot.Agent.ID == "*" || isGatewayAgent(snapshot) || (role != "" && role != "agent-proxy") {
			continue
		}
		class, _ := reportedResourceClass(snapshot)
		if class == "" || class == agentResourceTeam || managedTeam(snapshot.Agent.Groups) {
			continue
		}
		clients := snapshot.Report.Xray.Clients
		if clients == 0 {
			clients = snapshot.Report.Users
		}
		entry := agentInventoryEntry{
			ID:            snapshot.Agent.ID,
			NodeID:        snapshot.Agent.NodeID,
			Name:          snapshot.Agent.Name,
			ResourceClass: class,
			Status:        "unknown",
			LastSync:      snapshot.Report.Xray.LastSync,
		}
		if clients > 0 {
			cCount := clients
			entry.ClientCount = &cCount
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
