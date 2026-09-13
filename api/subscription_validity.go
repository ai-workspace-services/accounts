package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

type subscriptionValidityRequest struct {
	ValidFrom  *string `json:"validFrom"`
	ValidUntil *string `json:"validUntil"`
}

func parseSubscriptionDate(value *string) (*time.Time, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, nil
	}
	parsed, err := time.Parse("2006-01-02", strings.TrimSpace(*value))
	if err != nil {
		return nil, errors.New("date must use YYYY-MM-DD")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func subscriptionDateString(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format("2006-01-02")
}

// subscriptionValidityExpired treats the configured end date as inclusive.
// The UI only edits dates, so a subscription remains valid through 23:59:59
// UTC on validUntil and expires at the following midnight.
func subscriptionValidityExpired(user *store.User, now time.Time) bool {
	if user == nil || user.SubscriptionValidUntil == nil {
		return false
	}
	end := user.SubscriptionValidUntil.UTC()
	expiresAt := time.Date(end.Year(), end.Month(), end.Day()+1, 0, 0, 0, 0, time.UTC)
	return !now.UTC().Before(expiresAt)
}

// ensureExpiredSubscriptionDowngrade is deliberately lazy and event-driven:
// every config sync and agent refresh evaluates the expiry, so no Xray or
// Caddy restart is needed. It only replaces the quota group and never deletes
// or disables the user record.
func (h *handler) ensureExpiredSubscriptionDowngrade(ctx context.Context, user *store.User) error {
	if !subscriptionValidityExpired(user, time.Now().UTC()) {
		return nil
	}

	if store.MonthlyQuotaGroup(user) == store.MonthlyFreeQuotaLimitGroup {
		return nil
	}
	groups := make([]string, 0, len(user.Groups)+1)
	for _, group := range user.Groups {
		if group == store.MonthlyFreeQuotaLimitGroup ||
			group == store.MonthlyPlusQuotaLimitGroup ||
			group == store.MonthlyUnlimitedBetaQuotaGroup {
			continue
		}
		groups = append(groups, group)
	}
	groups = append(groups, store.MonthlyFreeQuotaLimitGroup)
	user.Groups = normalizeGroups(groups)
	if err := h.store.UpdateUser(ctx, user); err != nil {
		return err
	}
	// The group controls config eligibility; the billing profile must also be
	// reset so an expired paid account receives the actual Free 5GB allowance.
	return h.downgradeToFreePlan(ctx, user.ID)
}

func (h *handler) updateSubscriptionValidity(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite); !ok {
		return
	}

	userID := strings.TrimSpace(c.Param("userId"))
	if userID == "" {
		respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
		return
	}

	var req subscriptionValidityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	validFrom, err := parseSubscriptionDate(req.ValidFrom)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_date", err.Error())
		return
	}
	validUntil, err := parseSubscriptionDate(req.ValidUntil)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_date", err.Error())
		return
	}
	if validFrom != nil && validUntil != nil && validUntil.Before(*validFrom) {
		respondError(c, http.StatusBadRequest, "invalid_date_range", "validUntil must be on or after validFrom")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to fetch user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account subscription validity cannot be modified")
		return
	}

	user.SubscriptionValidFrom = validFrom
	user.SubscriptionValidUntil = validUntil
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to update subscription validity")
		return
	}
	if err := h.ensureExpiredSubscriptionDowngradeWithContext(c, user); err != nil {
		respondError(c, http.StatusInternalServerError, "downgrade_failed", "failed to apply expired subscription downgrade")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "subscription validity updated",
		"user":    sanitizeUser(user, nil),
		"subscriptionValidity": gin.H{
			"validFrom":  subscriptionDateString(user.SubscriptionValidFrom),
			"validUntil": subscriptionDateString(user.SubscriptionValidUntil),
		},
	})
}

func (h *handler) ensureExpiredSubscriptionDowngradeWithContext(c *gin.Context, user *store.User) error {
	return h.ensureExpiredSubscriptionDowngrade(c.Request.Context(), user)
}
