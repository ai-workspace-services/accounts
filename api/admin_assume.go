package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const (
	assumeSandboxEmail = sandboxUserEmail
)

func (h *handler) adminAssume(c *gin.Context) {
	adminUser, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !h.isRootAccount(adminUser) {
		respondError(c, http.StatusForbidden, "root_only", "root only")
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email != assumeSandboxEmail {
		respondError(c, http.StatusBadRequest, "invalid_target", "target is not allowed")
		return
	}

	// Resolve sandbox user.
	sandboxUser, err := h.store.GetUserByEmail(c.Request.Context(), assumeSandboxEmail)
	if err != nil {
		if err == store.ErrUserNotFound {
			respondError(c, http.StatusNotFound, "sandbox_missing", "sandbox user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "sandbox_lookup_failed", "failed to lookup sandbox user")
		return
	}

	// Renew sandbox access metadata if expired; the account UUID remains stable.
	if err := h.ensureSandboxProxyUUID(c.Request.Context(), sandboxUser); err != nil {
		respondError(c, http.StatusInternalServerError, "sandbox_uuid_rotation_failed", "failed to renew sandbox proxy access")
		return
	}

	sandboxToken, expiresAt, err := h.createSession(sandboxUser.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create sandbox session")
		return
	}

	slog.Info("admin assume sandbox",
		"event", "admin_assume",
		"actor_user_id", adminUser.ID,
		"actor_email", adminUser.Email,
		"target_user_id", sandboxUser.ID,
		"target_email", sandboxUser.Email,
	)

	// NOTE: cookies are intentionally NOT set by accounts.svc.plus.
	// The console BFF is responsible for setting host-scoped cookies.
	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"assumed":   assumeSandboxEmail,
		"token":     sandboxToken,
		"expiresAt": expiresAt.UTC(),
	})
}

func (h *handler) adminAssumeRevert(c *gin.Context) {
	adminUser, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !h.isRootAccount(adminUser) {
		respondError(c, http.StatusForbidden, "root_only", "root only")
		return
	}

	slog.Info("admin assume revert",
		"event", "admin_assume_revert",
		"actor_user_id", adminUser.ID,
		"actor_email", adminUser.Email,
		"target_email", assumeSandboxEmail,
	)

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handler) adminAssumeStatus(c *gin.Context) {
	adminUser, ok := h.requireAdminPermission(c, permissionAdminSettingsRead)
	if !ok {
		return
	}
	if !h.isRootAccount(adminUser) {
		respondError(c, http.StatusForbidden, "root_only", "root only")
		return
	}

	// NOTE: assume status is tracked via host-scoped cookies owned by console.svc.plus.
	// accounts.svc.plus cannot observe that state safely, so we only expose a stub.
	c.JSON(http.StatusOK, gin.H{"isAssuming": false, "target": ""})
}

// guard unused imports if build tags change.
var _ = context.Background
