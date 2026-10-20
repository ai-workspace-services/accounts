package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/overlay"
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

	identity, ok := h.resolveScopedAgent(c, *credIdentity, "")
	if !ok {
		return
	}
	if pendingTeamResponse(c) {
		return
	}
	scope, valid := agentResourceScope(identity.Groups)
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "agent_scope_invalid"})
		return
	}

	clients, revision, err := h.authorizedAgentClients(c.Request.Context(), identity.Groups)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_users_failed"})
		return
	}
	approvalStatus := ""
	if e, ok := teamEnrollment(c); ok {
		approvalStatus = e.Status
		revision = enrollmentRevision(e) + "-" + revision
	}
	c.Header("ETag", `"`+revision+`"`)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, agentproto.ClientListResponse{
		EnrollmentStatus: approvalStatus,
		Scope:            &scope,
		Clients:          clients,
		Total:            len(clients),
		Revision:         revision,
		GeneratedAt:      time.Now().UTC(),
	})
}

// groups must come from the authenticated credential or an approved persisted
// assignment, never directly from node request metadata.
// Requiring the argument prevents a caller from silently defaulting to public.
func (h *handler) authorizedAgentClients(ctx context.Context, groups []string) ([]xrayconfig.Client, string, error) {
	if _, valid := agentNetworkScope(groups); !valid {
		return nil, "", errors.New("invalid agent network scope")
	}
	teamStore, ok := h.store.(store.NetworkTeamStore)
	if !ok {
		return nil, "", errors.New("network team store unavailable")
	}
	clients := make([]xrayconfig.Client, 0, 16)
	scope, valid := agentResourceScope(groups)
	if !valid {
		return nil, "", errors.New("invalid agent resource scope")
	}
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		return nil, "", err
	}

	for _, u := range users {
		if err := h.archiveInactiveFreeUser(ctx, &u); err != nil {
			return nil, "", err
		}
		if err := h.ensureExpiredSubscriptionDowngrade(ctx, &u); err != nil {
			return nil, "", err
		}
	}

	// Read access pauses after reconciliation. Remaining quota may be negative,
	// but quota exhaustion alone must not withhold otherwise authorized clients.
	blocked, err := h.store.ListProxyBlockedAccountUUIDs(ctx)
	if err != nil {
		return nil, "", err
	}

	emails, err := h.store.ListBlacklist(ctx)
	if err != nil {
		return nil, "", err
	}
	blockedEmails := make(map[string]bool, len(emails))
	for _, email := range emails {
		blockedEmails[strings.ToLower(strings.TrimSpace(email))] = true
	}

	teams, err := teamStore.ListNetworkTeams(ctx)
	if err != nil {
		return nil, "", err
	}
	members, err := teamStore.ListNetworkMemberships(ctx)
	if err != nil {
		return nil, "", err
	}
	memberByUser := make(map[string]store.NetworkMembership, len(members))
	for _, member := range members {
		memberByUser[member.UserID] = member
	}
	now := time.Now().UTC()
	for _, u := range users {
		access := accessFromNetworkMembership(memberByUser[u.ID], teams, now)
		if !networkScopeAllows(access, groups) {
			continue
		}
		packageName, err := h.billingPackageName(ctx, &u)
		if err != nil {
			return nil, "", err
		}
		if !agentResourceScopeAllowsUser(scope, access, &u, packageName) {
			continue
		}
		if !u.Active {
			continue
		}
		// Every class, including demo accounts, honors the operator/billing blacklist.
		if blocked[strings.TrimSpace(u.ID)] || blockedEmails[strings.ToLower(strings.TrimSpace(u.Email))] {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(u.Email))

		if exhausted, err := h.quotaExhaustedForUser(ctx, &u); err != nil {
			return nil, "", err
		} else if exhausted {
			continue
		}
		// Sandbox is a special demo identity with expiring access metadata.
		// Its client ID remains the canonical account UUID across every node.
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
	payload, err := json.Marshal(struct {
		Scope   agentproto.UserScope `json:"scope"`
		Clients []xrayconfig.Client  `json:"clients"`
	}{Scope: scope, Clients: clients})
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
	credIdentity, ok := h.agentRegistry.Authenticate(token)
	if !ok || credIdentity == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}

	identity, ok := h.resolveScopedAgent(c, *credIdentity, "")
	if !ok {
		return
	}

	if e, managed := teamEnrollment(c); managed {
		revision := enrollmentRevision(e)
		if e.Status == "approved" {
			_, usersRevision, err := h.authorizedAgentClients(c.Request.Context(), identity.Groups)
			if err != nil {
				respondError(c, 503, "agent_users_unavailable", "team users unavailable")
				return
			}
			revision += "-" + usersRevision
		}
		// Explicit bounded polling rechecks assignment authority on every
		// connection; no captured identity survives a revoke/reassignment.
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-store")
		c.Header("X-Agent-Event-Mode", "poll")
		c.String(200, "event: users-changed\ndata: %s\n\n", revision)
		return
	}

	// The server's finite WriteTimeout protects ordinary API requests, but it
	// must not expire an authenticated SSE stream while it waits for changes.
	controller := http.NewResponseController(c.Writer)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "event_stream_unavailable"})
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
		_, revision, err := h.authorizedAgentClients(c.Request.Context(), identity.Groups)
		if err != nil {
			return
		}
		if revision != lastRevision {
			if _, err := c.Writer.WriteString("id: " + revision + "\nevent: users-changed\ndata: " + revision + "\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			lastRevision = revision
		}

		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			heartbeats++
			if heartbeats%3 == 0 {
				if _, err := c.Writer.WriteString(": keepalive\n\n"); err != nil {
					return
				}
				if err := controller.Flush(); err != nil {
					return
				}
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

	identity, ok := h.resolveScopedAgent(c, *credIdentity, report.AgentID)
	if !ok {
		return
	}

	if report.Overlay != nil && (len(report.Overlay.Paths) > 256 || len(report.Overlay.Capabilities) > 8 || len(report.Overlay.DeviceID) > 128 || len(report.Overlay.NetworkID) > 128) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_overlay_status"})
		return
	}
	// Ensure report uses the resolved agent id.
	report.AgentID = identity.ID
	h.agentRegistry.ReportStatus(identity, report)
	// A Gateway/One control agent may only refresh liveness for a device which
	// already exists through signed enrollment. This deliberately never creates
	// or reassigns an overlay device, so generic Agent telemetry cannot bypass
	// Zero ownership or tenant isolation.
	if h.overlayService != nil && (report.Role == overlay.RoleGateway || report.Role == overlay.RoleOne) {
		if err := h.overlayService.RecordRuntimeObservation(c.Request.Context(), overlay.RuntimeObservationRequest{
			NetworkID: report.Xray.NetworkID,
			DeviceID:  report.Xray.NodeID,
			Role:      report.Role,
		}); err != nil {
			if !errors.Is(err, overlay.ErrNotFound) && !errors.Is(err, overlay.ErrInvalidInput) {
				c.Error(err)
			}
		}
	}
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
