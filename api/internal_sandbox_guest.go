package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// internalSandboxGuest returns 410 Gone because the sandbox account has been retired.
//
// Protected by InternalAuthMiddleware via /api/internal.
func (h *handler) internalSandboxGuest(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{
		"error":   "sandbox_retired",
		"message": "sandbox demo account has been retired",
	})
}

var (
	_ = errors.New
	_ = strings.TrimSpace
	_ = time.Now
	_ = store.ErrUserNotFound
)
