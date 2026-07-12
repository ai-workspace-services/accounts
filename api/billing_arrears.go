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
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite); !ok {
		return
	}
	accountUUID := strings.TrimSpace(c.Param("accountUUID"))
	if accountUUID == "" {
		respondError(c, http.StatusBadRequest, "account_uuid_required", "account uuid is required")
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

	state.Arrears = false
	state.ArrearsSince = nil
	state.ThrottleState = "normal"
	state.SuspendState = "active"
	state.EffectiveAt = time.Now().UTC()
	if err := h.store.UpsertAccountQuotaState(c.Request.Context(), state); err != nil {
		respondError(c, http.StatusInternalServerError, "quota_state_save_failed", "failed to clear arrears")
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
