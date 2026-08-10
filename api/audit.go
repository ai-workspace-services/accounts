package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// Operator writes must say why. Anything that can move entitlements, quota,
// balance or pricing is answerable later, and "who changed this and why" is
// not reconstructable after the fact — so the reason is collected at the
// point of change and rejected at the API boundary when missing. The frontend
// disables the submit button too, but that is UX, not enforcement.
const auditReasonMaxLen = 500

// recordAudit writes one audit entry. Unlike publishBillingEvent it returns
// the error instead of only logging it: an unrecorded change to money or
// entitlements is not an acceptable outcome, so callers fail the request.
func (h *handler) recordAudit(ctx context.Context, actorUUID, action string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	entry := &store.AuditLog{
		Action:    action,
		ActorUUID: strings.TrimSpace(actorUUID),
		Details:   details,
	}
	if err := h.store.InsertAuditLog(ctx, entry); err != nil {
		slog.Error("failed to record audit entry", "err", err, "action", action, "actor", actorUUID)
		return err
	}
	return nil
}

// auditDetails builds the standard payload shape every entry shares, so the
// audit stream stays queryable rather than becoming a bag of ad-hoc keys.
func auditDetails(targetUUID, reason string, before, after map[string]any) map[string]any {
	details := map[string]any{
		"target_uuid": strings.TrimSpace(targetUUID),
		"reason":      strings.TrimSpace(reason),
	}
	if before != nil {
		details["before"] = before
	}
	if after != nil {
		details["after"] = after
	}
	return details
}

// requireReason validates the operator-supplied justification. Returns the
// trimmed value and false when the request should be rejected.
func requireReason(c *gin.Context, reason string) (string, bool) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		respondError(c, http.StatusBadRequest, "reason_required",
			"a reason is required for this change and is recorded in the audit log")
		return "", false
	}
	if len(trimmed) > auditReasonMaxLen {
		respondError(c, http.StatusBadRequest, "reason_too_long",
			"reason must be at most 500 characters")
		return "", false
	}
	return trimmed, true
}

// adminListAuditLogs serves the audit trail. Read-only, so it takes the read
// permission rather than the write one.
func (h *handler) adminListAuditLogs(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsRead); !ok {
		return
	}

	filter := store.AuditLogFilter{
		ActionPrefix: strings.TrimSpace(c.Query("action")),
		ActorUUID:    strings.TrimSpace(c.Query("actor")),
		TargetUUID:   strings.TrimSpace(c.Query("target")),
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			filter.Limit = parsed
		}
	}
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			filter.Offset = parsed
		}
	}

	entries, err := h.store.ListAuditLogs(c.Request.Context(), filter)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "audit_logs_unavailable", "failed to load audit logs")
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries})
}
