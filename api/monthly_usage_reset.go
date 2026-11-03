package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"time"
)

func (h *handler) adminResetMonthlyUsage(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, "")
	if !ok {
		return
	}
	if !subscriptionImportRoleAllowed(actor) {
		respondError(c, 403, "forbidden", "operations role required")
		return
	}
	user, ok := h.resolveTargetUser(c)
	if !ok {
		return
	}
	var req struct {
		Email  string `json:"email"`
		Reason string `json:"reason"`
	}
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "email and reason required")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Email), strings.TrimSpace(user.Email)) {
		respondError(c, 409, "account_email_mismatch", "selected account email does not match")
		return
	}
	q, err := h.store.ResetMonthlyUsage(c.Request.Context(), user.ID, actor.ID, user.Email, reason, time.Now().UTC())
	if err != nil {
		respondError(c, 409, "usage_reset_failed", "account quota could not be reset; refresh account and retry")
		return
	}
	c.JSON(http.StatusOK, gin.H{"accountUuid": user.ID, "email": user.Email, "quotaState": q, "expiresAt": q.PeriodEnd, "effectiveFrom": q.PeriodStart, "timezone": "UTC"})
}
