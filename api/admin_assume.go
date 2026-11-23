package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
	if email == assumeSandboxEmail {
		respondError(c, http.StatusGone, "sandbox_retired", "sandbox account has been retired")
		return
	}
	respondError(c, http.StatusBadRequest, "invalid_target", "target is not allowed")
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
