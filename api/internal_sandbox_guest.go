package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// internalSandboxGuest returns the sandbox user's proxy access metadata for the
// Console Guest/Demo experience.
//
// Protected by InternalAuthMiddleware via /api/internal.
func (h *handler) internalSandboxGuest(c *gin.Context) {
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "store_not_configured"})
		return
	}

	user, err := h.store.GetUserByEmail(c.Request.Context(), sandboxUserEmail)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "sandbox_missing"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sandbox_lookup_failed"})
		return
	}

	if err := h.ensureSandboxProxyUUID(c.Request.Context(), user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sandbox_uuid_rotation_failed"})
		return
	}

	proxyUUID := strings.TrimSpace(user.ProxyUUID)
	expiresAt := ""
	if user.ProxyUUIDExpiresAt != nil {
		expiresAt = user.ProxyUUIDExpiresAt.UTC().Format(time.RFC3339)
	}
	if proxyUUID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "proxy_uuid_unavailable",
			"message": "proxy UUID is not provisioned for the sandbox account",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"email":              sandboxUserEmail,
		"proxyUuid":          proxyUUID,
		"proxyUuidExpiresAt": expiresAt,
	})
}
