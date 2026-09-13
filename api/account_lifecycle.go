package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const freeAccountArchiveAfter = 100 * 24 * time.Hour

func userLastActivity(user *store.User) time.Time {
	if user == nil {
		return time.Time{}
	}
	if user.LastActiveAt != nil {
		return user.LastActiveAt.UTC()
	}
	if !user.UpdatedAt.IsZero() {
		return user.UpdatedAt.UTC()
	}
	return user.CreatedAt.UTC()
}

// archiveInactiveFreeUser is called from the existing refresh paths. It is
// intentionally lazy/event-driven, so adding this policy does not require a
// new worker or an Xray/Caddy restart. Paid/subscribed users are protected.
func (h *handler) archiveInactiveFreeUser(ctx context.Context, user *store.User) error {
	if user == nil || !user.Active || store.MonthlyQuotaGroup(user) != store.MonthlyFreeQuotaLimitGroup {
		return nil
	}
	lastActivity := userLastActivity(user)
	if lastActivity.IsZero() || time.Since(lastActivity) < freeAccountArchiveAfter {
		return nil
	}

	now := time.Now().UTC()
	user.Active = false
	user.ArchivedAt = &now
	if err := h.store.UpdateUser(ctx, user); err != nil {
		return err
	}
	state := &store.AccountQuotaState{AccountUUID: user.ID, ThrottleState: "normal", SuspendState: "active", ProxyAccessState: "paused"}
	if existing, err := h.store.GetAccountQuotaState(ctx, user.ID); err == nil && existing != nil {
		state = existing
		state.ProxyAccessState = "paused"
	}
	return h.store.UpsertAccountQuotaState(ctx, state)
}

func (h *handler) touchUserActivity(ctx context.Context, user *store.User) {
	if user == nil || !user.Active {
		return
	}
	now := time.Now().UTC()
	if user.LastActiveAt != nil && now.Sub(user.LastActiveAt.UTC()) < 24*time.Hour {
		return
	}
	user.LastActiveAt = &now
	if err := h.store.UpdateUser(ctx, user); err != nil {
		// Activity telemetry must not turn a successful request into a failure.
		slog.Warn("failed to record user activity", "userID", user.ID, "err", err)
	}
}

func (h *handler) enqueueReactivationCode(ctx context.Context, user *store.User, locale mailLocale) error {
	code, err := h.newVerificationCode()
	if err != nil {
		return err
	}
	ttl := h.resetTTL
	if ttl <= 0 {
		ttl = defaultPasswordResetTTL
	}
	expiresAt := time.Now().Add(ttl)
	email := strings.ToLower(strings.TrimSpace(user.Email))
	h.reactivationCodeMu.Lock()
	h.reactivationCodes[email] = accountReactivationCode{userID: user.ID, email: email, code: code, expiresAt: expiresAt}
	h.reactivationCodeMu.Unlock()
	copy := copyFor(locale)
	plainBody, htmlBody := renderTransactionalEmail(transactionalEmail{
		Locale: locale, Greeting: fmt.Sprintf(copy.greetingNamed, strings.TrimSpace(user.Name)),
		Intro: copy.introVerify, CodeLabel: copy.labelCode, Code: code, CodeStyle: codeStyleDigits,
		Expiry: expiryLine(locale, expiresAt, ttl), Reassure: copy.reassureIgnore,
	})
	if err := h.emailSender.Send(ctx, EmailMessage{To: []string{email}, Subject: copy.subjectRegister, PlainBody: plainBody, HTMLBody: htmlBody}); err != nil {
		h.removeReactivationCode(email)
		return err
	}
	return nil
}

func (h *handler) removeReactivationCode(email string) {
	h.reactivationCodeMu.Lock()
	delete(h.reactivationCodes, strings.ToLower(strings.TrimSpace(email)))
	h.reactivationCodeMu.Unlock()
}

func (h *handler) recordReactivationCodeFailure(email string) time.Time {
	email = strings.ToLower(strings.TrimSpace(email))
	h.reactivationCodeMu.Lock()
	defer h.reactivationCodeMu.Unlock()
	entry, ok := h.reactivationCodes[email]
	if !ok {
		return time.Time{}
	}
	entry.failedAttempts++
	if entry.failedAttempts >= maxMFAVerificationAttempts {
		entry.lockedUntil = time.Now().Add(defaultMFALockoutDuration)
		entry.failedAttempts = 0
	}
	h.reactivationCodes[email] = entry
	return entry.lockedUntil
}

func (h *handler) reactivateAccount(c *gin.Context) {
	var req verificationCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	code := strings.TrimSpace(req.Code)
	if email == "" || len(code) != 6 {
		respondError(c, http.StatusBadRequest, "invalid_code", "email and a 6 digit verification code are required")
		return
	}
	h.reactivationCodeMu.RLock()
	entry, ok := h.reactivationCodes[email]
	h.reactivationCodeMu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) || time.Now().Before(entry.lockedUntil) {
		respondError(c, http.StatusUnauthorized, "invalid_code", "verification code is invalid or expired")
		return
	}
	if entry.code != code {
		if retryAt := h.recordReactivationCodeFailure(email); !retryAt.IsZero() {
			respondVerificationLocked(c, retryAt)
			return
		}
		respondError(c, http.StatusUnauthorized, "invalid_code", "verification code is invalid or expired")
		return
	}
	user, err := h.store.GetUserByID(c.Request.Context(), entry.userID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(user.Email), email) || user.ArchivedAt == nil {
		respondError(c, http.StatusUnauthorized, "invalid_code", "verification code is invalid or expired")
		return
	}
	user.Active = true
	user.ArchivedAt = nil
	now := time.Now().UTC()
	user.LastActiveAt = &now
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "account_reactivation_failed", "failed to reactivate account")
		return
	}
	h.removeReactivationCode(email)
	token, expiresAt, err := h.createSession(user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
		return
	}
	h.setSessionCookie(c, token, expiresAt)
	c.JSON(http.StatusOK, gin.H{"message": "account reactivated", "token": token, "expiresAt": expiresAt.UTC(), "user": sanitizeUser(user, nil)})
}

func (h *handler) hasActiveSubscription(ctx context.Context, userID string) (bool, error) {
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, subscription := range subscriptions {
		if subscription.Status == "active" || subscription.Status == "trialing" || subscription.Status == "past_due" {
			return true, nil
		}
	}
	return false, nil
}
