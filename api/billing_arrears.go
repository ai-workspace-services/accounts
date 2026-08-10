package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// adminClearArrears is the manual recovery half of the P1.5 dunning path
// (the automatic half is invoice.paid -> resetQuotaForPlan). Support/ops use
// it after an out-of-band settlement: it clears the arrears episode and lifts
// throttle/suspend so the next agent sync restores access. It deliberately
// does not touch quota or balance — those stay whatever rating left them at.
func (h *handler) adminClearArrears(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	accountUUID := strings.TrimSpace(c.Param("accountUUID"))
	if accountUUID == "" {
		respondError(c, http.StatusBadRequest, "account_uuid_required", "account uuid is required")
		return
	}

	// Lifting a suspension is an operator decision that has to be explainable
	// later, so it takes a reason like every other ops write. Accepted in the
	// body; the endpoint previously took none, so callers must be updated.
	var req struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&req)
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}

	state, err := h.store.GetAccountQuotaState(c.Request.Context(), accountUUID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "quota_state_not_found", "no quota state for account")
			return
		}
		respondError(c, http.StatusInternalServerError, "quota_state_unavailable", "failed to load quota state")
		return
	}

	before := map[string]any{
		"arrears":        state.Arrears,
		"throttle_state": state.ThrottleState,
		"suspend_state":  state.SuspendState,
	}

	state.Arrears = false
	state.ArrearsSince = nil
	state.ThrottleState = "normal"
	state.SuspendState = "active"
	state.EffectiveAt = time.Now().UTC()
	if err := h.store.UpsertAccountQuotaState(c.Request.Context(), state); err != nil {
		respondError(c, http.StatusInternalServerError, "quota_state_save_failed", "failed to clear arrears")
		return
	}

	after := map[string]any{
		"arrears":        false,
		"throttle_state": state.ThrottleState,
		"suspend_state":  state.SuspendState,
	}
	if err := h.recordAudit(c.Request.Context(), actor.ID, store.AuditActionArrearsClear,
		auditDetails(accountUUID, reason, before, after)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"arrears were cleared but the audit entry could not be written")
		return
	}

	h.publishBillingEvent(c.Request.Context(), &store.BillingEvent{
		Type: "arrears_cleared", UserID: accountUUID,
	})

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":   accountUUID,
		"arrears":       false,
		"throttleState": state.ThrottleState,
		"suspendState":  state.SuspendState,
	})
}
