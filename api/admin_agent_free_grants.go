package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/agentserver"
	"account/internal/store"
)

type adminAgentFreeGrantEntry struct {
	NodeID    string     `json:"nodeId"`
	AccountID string     `json:"accountId"`
	Email     string     `json:"email"`
	Status    string     `json:"status"`
	State     string     `json:"state"`
	ExpiresAt time.Time  `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	ActorID   string     `json:"actorId,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type upsertAgentFreeGrantRequest struct {
	NodeID        string `json:"nodeId"`
	AccountID     string `json:"accountId,omitempty"`
	Email         string `json:"email,omitempty"`
	ExpiresInDays int    `json:"expiresInDays,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
	Reason        string `json:"reason"`
}

type revokeAgentFreeGrantRequest struct {
	NodeID    string `json:"nodeId"`
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"email,omitempty"`
	Reason    string `json:"reason"`
}

func (h *handler) requireGrantStore() (store.AgentFreeGrantStore, bool) {
	if h.store == nil {
		return nil, false
	}
	grantStore, ok := h.store.(store.AgentFreeGrantStore)
	return grantStore, ok
}

func (h *handler) isEnrolledCustomNode(nodeID string) bool {
	nodeID = strings.TrimSpace(nodeID)
	if !store.ValidAgentFreeGrantNodeID(nodeID) {
		return false
	}
	foundAny := false
	if inspector, ok := h.agentRegistry.(interface{ Agents() []agentserver.Identity }); ok {
		for _, a := range inspector.Agents() {
			if a.NodeID != "" || a.ID != "*" {
				foundAny = true
			}
			if (a.NodeID == nodeID || a.ID == nodeID) && a.NodeID != "" {
				scope, valid := agentResourceScope(a.Groups)
				if valid && scope.ResourceClass == agentResourceCustom {
					return true
				}
			}
		}
	}
	if h.agentStatusReader != nil {
		for _, s := range h.agentStatusReader.Statuses() {
			if s.Agent.NodeID != "" || s.Agent.ID != "*" {
				foundAny = true
			}
			if (s.Agent.NodeID == nodeID || s.Agent.ID == nodeID) && s.Agent.NodeID != "" {
				scope, valid := agentResourceScope(s.Agent.Groups)
				if valid && scope.ResourceClass == agentResourceCustom {
					return true
				}
			}
		}
	}
	// If no agents are loaded (e.g. in test environment without registry), fall back to format check.
	return !foundAny
}

func (h *handler) computeGrantState(grant store.AgentFreeGrant, now time.Time) string {
	var nodeLastSync *time.Time
	var nodeHealthy bool
	if h.agentStatusReader != nil {
		for _, s := range h.agentStatusReader.Statuses() {
			if (s.Agent.NodeID == grant.NodeID || s.Agent.ID == grant.NodeID) && !s.UpdatedAt.IsZero() {
				nodeHealthy = s.Report.Healthy && s.Report.Xray.Running
				if s.Report.Xray.LastSync != nil {
					t := s.Report.Xray.LastSync.UTC()
					nodeLastSync = &t
				}
				break
			}
		}
	}

	syncedAfter := func(target time.Time) bool {
		if nodeLastSync == nil || !nodeHealthy {
			return false
		}
		return !nodeLastSync.Before(target)
	}

	if grant.Status == store.AgentFreeGrantStatusRevoked {
		threshold := grant.UpdatedAt
		if grant.RevokedAt != nil && !grant.RevokedAt.IsZero() {
			threshold = *grant.RevokedAt
		}
		if syncedAfter(threshold) {
			return "revoked"
		}
		return "pending_revocation_ack"
	}

	if !now.Before(grant.ExpiresAt) {
		if syncedAfter(grant.ExpiresAt) {
			return "expired"
		}
		return "pending_revocation_ack"
	}

	if syncedAfter(grant.UpdatedAt) {
		return "active"
	}
	return "pending"
}

func (h *handler) adminListAgentFreeGrants(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus)
	if !ok {
		return
	}
	if !store.IsAdminRole(actor.Role) && !store.IsOperatorRole(actor.Role) {
		respondError(c, http.StatusForbidden, "forbidden", "agent grant inventory requires an administrative role")
		return
	}
	grantStore, ok := h.requireGrantStore()
	if !ok {
		respondError(c, http.StatusServiceUnavailable, "agent_grants_unavailable", "grant store is not configured")
		return
	}

	nodeID := strings.TrimSpace(c.Query("nodeId"))
	if nodeID != "" && !store.ValidAgentFreeGrantNodeID(nodeID) {
		respondError(c, http.StatusBadRequest, "invalid_node_id", "node ID is invalid")
		return
	}

	grants, err := grantStore.ListAgentFreeGrants(c.Request.Context(), nodeID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "list_grants_failed", "failed to list temporary free grants")
		return
	}

	now := time.Now().UTC()
	entries := make([]adminAgentFreeGrantEntry, 0, len(grants))
	for _, g := range grants {
		entries = append(entries, adminAgentFreeGrantEntry{
			NodeID:    g.NodeID,
			AccountID: g.AccountID,
			Email:     g.Email,
			Status:    g.Status,
			State:     h.computeGrantState(g, now),
			ExpiresAt: g.ExpiresAt,
			CreatedAt: g.CreatedAt,
			UpdatedAt: g.UpdatedAt,
			RevokedAt: g.RevokedAt,
			ActorID:   g.ActorID,
			Reason:    g.Reason,
		})
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"grants": entries})
}

func (h *handler) requireAdminGrantsWritePermission(c *gin.Context) (*store.User, bool) {
	actor, ok := h.requireAdminPermission(c, permissionAdminAgentsGrantsWrite)
	if ok {
		return actor, true
	}
	// Fall back to general admin.settings.write if permitted.
	return h.requireAdminPermission(c, permissionAdminSettingsWrite)
}

func (h *handler) adminUpsertAgentFreeGrant(c *gin.Context) {
	actor, ok := h.requireAdminGrantsWritePermission(c)
	if !ok {
		return
	}
	grantStore, ok := h.requireGrantStore()
	if !ok {
		respondError(c, http.StatusServiceUnavailable, "agent_grants_unavailable", "grant store is not configured")
		return
	}

	var req upsertAgentFreeGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}

	nodeID := strings.TrimSpace(req.NodeID)
	if !store.ValidAgentFreeGrantNodeID(nodeID) {
		respondError(c, http.StatusBadRequest, "invalid_node_id", "valid node ID is required")
		return
	}
	if !h.isEnrolledCustomNode(nodeID) {
		respondError(c, http.StatusBadRequest, "node_not_custom", "temporary Free grants are only allowed on bound Custom nodes")
		return
	}

	var targetUser *store.User
	var err error
	accountID := strings.TrimSpace(req.AccountID)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if accountID != "" {
		targetUser, err = h.store.GetUserByID(c.Request.Context(), accountID)
	} else if email != "" {
		targetUser, err = h.store.GetUserByEmail(c.Request.Context(), email)
	} else {
		respondError(c, http.StatusBadRequest, "user_required", "either accountId or email is required")
		return
	}

	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "target user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to lookup user")
		return
	}

	if !targetUser.Active || targetUser.ArchivedAt != nil {
		respondError(c, http.StatusConflict, "user_inactive", "target user is inactive or archived")
		return
	}

	access, err := h.networkAccessForUser(c.Request.Context(), targetUser)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "network_access_failed", "failed to evaluate network access")
		return
	}
	if access.Mode == "dedicated" {
		respondError(c, http.StatusConflict, "dedicated_user_ineligible", "team members cannot receive temporary public Free grants")
		return
	}

	pkgName, err := h.billingPackageName(c.Request.Context(), targetUser)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "billing_lookup_failed", "failed to evaluate billing package")
		return
	}
	effectiveClass := effectiveAgentResourceClass(access, targetUser, pkgName)
	if effectiveClass != agentResourceFree {
		respondError(c, http.StatusConflict, "non_free_user", "only Free plan accounts are eligible for temporary Free grants")
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	var expiresAt time.Time
	if req.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_expires_at", "expiresAt must be RFC3339 format")
			return
		}
		expiresAt = parsed.UTC().Truncate(time.Millisecond)
	} else {
		if req.ExpiresInDays > 5 {
			respondError(c, http.StatusBadRequest, "grant_ttl_exceeded", "temporary Free grant cannot exceed 5 days")
			return
		}
		days := req.ExpiresInDays
		if days <= 0 {
			days = 3
		}
		expiresAt = now.Add(time.Duration(days) * 24 * time.Hour)
	}

	if !expiresAt.After(now) {
		respondError(c, http.StatusBadRequest, "invalid_expires_at", "expiresAt must be in the future")
		return
	}
	if expiresAt.Sub(now) > store.MaxAgentFreeGrantTTL+time.Minute {
		respondError(c, http.StatusBadRequest, "grant_ttl_exceeded", "temporary Free grant cannot exceed 5 days")
		return
	}

	isRenew := false
	existingGrant, exists, err := grantStore.GetAgentFreeGrant(c.Request.Context(), nodeID, targetUser.ID)
	if err == nil && exists && existingGrant.Status == store.AgentFreeGrantStatusActive && now.Before(existingGrant.ExpiresAt) {
		isRenew = true
	}

	action := store.AuditActionAgentFreeGrantCreate
	if isRenew {
		action = store.AuditActionAgentFreeGrantRenew
	}

	audit := &store.AuditLog{
		UUID:      uuid.NewString(),
		Action:    action,
		ActorUUID: actor.ID,
		CreatedAt: now,
		Details: map[string]any{
			"reason":  reason,
			"node_id": nodeID,
		},
	}

	grant := store.AgentFreeGrant{
		NodeID:    nodeID,
		AccountID: targetUser.ID,
		Email:     targetUser.Email,
		Status:    store.AgentFreeGrantStatusActive,
		ExpiresAt: expiresAt,
		UpdatedAt: now,
		ActorID:   actor.ID,
		Reason:    reason,
	}

	saved, err := grantStore.PutAgentFreeGrant(c.Request.Context(), grant, audit)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "grant_save_failed", "failed to save temporary Free grant")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "grant_applied",
		"grant": adminAgentFreeGrantEntry{
			NodeID:    saved.NodeID,
			AccountID: saved.AccountID,
			Email:     saved.Email,
			Status:    saved.Status,
			State:     h.computeGrantState(saved, now),
			ExpiresAt: saved.ExpiresAt,
			CreatedAt: saved.CreatedAt,
			UpdatedAt: saved.UpdatedAt,
			RevokedAt: saved.RevokedAt,
			ActorID:   saved.ActorID,
			Reason:    saved.Reason,
		},
	})
}

func (h *handler) adminRevokeAgentFreeGrant(c *gin.Context) {
	actor, ok := h.requireAdminGrantsWritePermission(c)
	if !ok {
		return
	}
	grantStore, ok := h.requireGrantStore()
	if !ok {
		respondError(c, http.StatusServiceUnavailable, "agent_grants_unavailable", "grant store is not configured")
		return
	}

	var req revokeAgentFreeGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		req.NodeID = c.Query("nodeId")
		req.AccountID = c.Query("accountId")
		req.Email = c.Query("email")
		req.Reason = c.Query("reason")
	}

	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}

	nodeID := strings.TrimSpace(req.NodeID)
	if !store.ValidAgentFreeGrantNodeID(nodeID) {
		respondError(c, http.StatusBadRequest, "invalid_node_id", "valid node ID is required")
		return
	}

	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" && req.Email != "" {
		u, err := h.store.GetUserByEmail(c.Request.Context(), strings.ToLower(strings.TrimSpace(req.Email)))
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				respondError(c, http.StatusNotFound, "user_not_found", "target user not found")
				return
			}
			respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to lookup user")
			return
		}
		accountID = u.ID
	}
	if accountID == "" {
		respondError(c, http.StatusBadRequest, "user_required", "accountId or email is required")
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	revoked, err := grantStore.RevokeAgentFreeGrant(c.Request.Context(), nodeID, accountID, actor.ID, reason, now)
	if err != nil {
		if errors.Is(err, store.ErrAgentFreeGrantNotFound) {
			respondError(c, http.StatusNotFound, "grant_not_found", "temporary Free grant not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "grant_revoke_failed", "failed to revoke temporary Free grant")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "grant_revoked",
		"grant": adminAgentFreeGrantEntry{
			NodeID:    revoked.NodeID,
			AccountID: revoked.AccountID,
			Email:     revoked.Email,
			Status:    revoked.Status,
			State:     h.computeGrantState(revoked, now),
			ExpiresAt: revoked.ExpiresAt,
			CreatedAt: revoked.CreatedAt,
			UpdatedAt: revoked.UpdatedAt,
			RevokedAt: revoked.RevokedAt,
			ActorID:   revoked.ActorID,
			Reason:    revoked.Reason,
		},
	})
}
