package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
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

	clients, revision, err := h.authorizedAgentClients(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_users_failed"})
		return
	}
	c.Header("ETag", `"`+revision+`"`)
	c.JSON(http.StatusOK, agentproto.ClientListResponse{
		Clients:     clients,
		Total:       len(clients),
		Revision:    revision,
		GeneratedAt: time.Now().UTC(),
	})
}

func (h *handler) authorizedAgentClients(ctx context.Context) ([]xrayconfig.Client, string, error) {
	clients := make([]xrayconfig.Client, 0, 16)
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		return nil, "", err
	}

	blocked, err := h.store.ListProxyBlockedAccountUUIDs(ctx)
	if err != nil {
		return nil, "", err
	}

	for _, u := range users {
		if !u.Active {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(u.Email))

		// Sandbox is a special demo identity with expiring access metadata.
		// Its client ID remains the canonical account UUID across every node.
		// It is exempt from the arrears suspension filter below for the same
		// reason: the Guest experience must not depend on billing state.
		if email == sandboxUserEmail {
			sandboxUser := u
			_ = h.ensureSandboxProxyUUID(ctx, &sandboxUser)

			id := strings.TrimSpace(sandboxUser.ProxyUUID)
			if id != "" {
				clients = append(clients, xrayconfig.Client{
					ID:    id,
					Email: strings.ToLower(strings.TrimSpace(sandboxUser.Email)),
					Flow:  xrayconfig.DefaultFlow,
				})
			}
			continue
		}

		// Full proxy access requires a verified email (which also activates the
		// trial). OAuth users are Active immediately but stay EmailVerified=false
		// until they complete the email round trip, so they get no xray client
		// until then. Sandbox is exempt (handled above).
		if !u.EmailVerified {
			continue
		}

		// Pause configuration synchronization for exhausted monthly quota,
		// prolonged billing suspension, or an operator pause. The account and its
		// billing history remain intact; only the node-local Xray credential is
		// withheld until the pause clears. Caddy cannot apply this policy because
		// the VLESS account identity is visible only inside Xray.
		if blocked[strings.TrimSpace(u.ID)] {
			continue
		}

		id := strings.TrimSpace(u.ProxyUUID)
		if id == "" {
			continue
		}
		clients = append(clients, xrayconfig.Client{
			ID:    id,
			Email: strings.ToLower(strings.TrimSpace(u.Email)),
			Flow:  xrayconfig.DefaultFlow,
		})
	}

	sort.Slice(clients, func(i, j int) bool { return clients[i].ID < clients[j].ID })
	payload, err := json.Marshal(clients)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(payload)
	return clients, hex.EncodeToString(sum[:]), nil
}

func (h *handler) watchAgentUsers(c *gin.Context) {
	if h.agentRegistry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent_registry_unavailable"})
		return
	}
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_token"})
		return
	}
	if identity, ok := h.agentRegistry.Authenticate(token); !ok || identity == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	lastRevision := strings.Trim(strings.TrimSpace(c.GetHeader("Last-Event-ID")), `"`)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	heartbeats := 0

	for {
		_, revision, err := h.authorizedAgentClients(c.Request.Context())
		if err != nil {
			return
		}
		if revision != lastRevision {
			_, _ = c.Writer.WriteString("id: " + revision + "\n")
			_, _ = c.Writer.WriteString("event: users-changed\n")
			_, _ = c.Writer.WriteString("data: " + revision + "\n\n")
			c.Writer.Flush()
			lastRevision = revision
		}

		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			heartbeats++
			if heartbeats%3 == 0 {
				_, _ = c.Writer.WriteString(": keepalive\n\n")
				c.Writer.Flush()
			}
		}
	}
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
