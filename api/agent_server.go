package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"account/internal/xrayconfig"
)

const agentIDHeader = "X-Agent-ID"

func (h *handler) listAgentUsers(c *gin.Context) {
	if h.agentRegistry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent_registry_unavailable"})
		return
	}

	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_token"})
		return
	}

	credIdentity, ok := h.agentRegistry.Authenticate(token)
	if !ok || credIdentity == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}

	agentID := strings.TrimSpace(c.GetHeader(agentIDHeader))
	if agentID == "" {
		agentID = strings.TrimSpace(c.Query("agentId"))
	}
	if agentID == "" {
		agentID = credIdentity.ID
	}

	identity := *credIdentity
	if agentID != "" && agentID != identity.ID {
		// Shared token scenario: register a concrete agent id so sandbox bindings can target it.
		identity = h.agentRegistry.RegisterAgent(agentID, identity.Groups)
	}

	now := time.Now().UTC()
	clients := make([]xrayconfig.Client, 0, 16)

	users, err := h.store.ListUsers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_users_failed"})
		return
	}

	for _, u := range users {
		if !u.Active {
			continue
		}
		suspended, err := h.store.IsAccountSuspended(c.Request.Context(), u.ID, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing_state_unavailable"})
			return
		}
		if suspended {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(u.Email))

		// Sandbox is a special demo identity with a rotating proxy UUID.
		// Always include it (and rotate on read if needed), so every node/region
		// can sync a consistent sandbox client for the Guest experience.
		if email == sandboxUserEmail {
			sandboxUser := u
			_ = h.ensureSandboxProxyUUID(c.Request.Context(), &sandboxUser)

			id := strings.TrimSpace(sandboxUser.ProxyUUID)
			if id == "" {
				id = strings.TrimSpace(sandboxUser.ID)
			}
			if id != "" {
				clients = append(clients, xrayconfig.Client{
					ID:    id,
					Email: strings.ToLower(strings.TrimSpace(sandboxUser.Email)),
					Flow:  xrayconfig.DefaultFlow,
				})
			}
			continue
		}

		id := strings.TrimSpace(u.ProxyUUID)
		if id == "" {
			id = strings.TrimSpace(u.ID)
		}
		if id == "" {
			continue
		}
		clients = append(clients, xrayconfig.Client{
			ID:    id,
			Email: strings.ToLower(strings.TrimSpace(u.Email)),
			Flow:  xrayconfig.DefaultFlow,
		})
	}

	c.JSON(http.StatusOK, agentproto.ClientListResponse{
		Clients:     clients,
		Total:       len(clients),
		GeneratedAt: now,
	})
}

func (h *handler) reportAgentStatus(c *gin.Context) {
	if h.agentRegistry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent_registry_unavailable"})
		return
	}

	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_token"})
		return
	}

	credIdentity, ok := h.agentRegistry.Authenticate(token)
	if !ok || credIdentity == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}

	var report agentproto.StatusReport
	if err := c.ShouldBindJSON(&report); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	agentID := strings.TrimSpace(report.AgentID)
	if agentID == "" {
		agentID = strings.TrimSpace(c.GetHeader(agentIDHeader))
	}
	if agentID == "" {
		agentID = credIdentity.ID
	}

	identity := *credIdentity
	if agentID != "" && agentID != identity.ID {
		identity = h.agentRegistry.RegisterAgent(agentID, identity.Groups)
	}

	// Ensure report uses the resolved agent id.
	report.AgentID = identity.ID
	h.agentRegistry.ReportStatus(identity, report)
	if h.store != nil {
		nodeID := strings.TrimSpace(report.Xray.NodeID)
		if nodeID == "" {
			nodeID = identity.ID
		}
		_ = h.store.UpsertNodeHealthSnapshot(c.Request.Context(), &store.NodeHealthSnapshot{
			NodeID:       nodeID,
			Region:       strings.TrimSpace(report.Xray.Region),
			LineCode:     strings.TrimSpace(report.Xray.LineCode),
			PricingGroup: strings.TrimSpace(report.Xray.PricingGroup),
			StatsEnabled: report.Xray.StatsEnabled,
			XrayRevision: strings.TrimSpace(report.Xray.XrayRevision),
			Healthy:      report.Healthy,
			SampledAt:    time.Now().UTC(),
		})
	}

	c.Status(http.StatusNoContent)
}

var _ = agentserver.Identity{}
