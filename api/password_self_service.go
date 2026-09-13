package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"account/internal/store"
)

type passwordResetCodeRequest struct {
	Email string `json:"email"`
}

type passwordResetCodeConfirmRequest struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

type mfaPasswordResetRequest struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (h *handler) requestPasswordResetCode(c *gin.Context) {
	if hasQueryParameter(c, "email") {
		respondError(c, http.StatusBadRequest, "email_in_query", "email must be sent in the request body")
		return
	}
	var req passwordResetCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		respondError(c, http.StatusBadRequest, "email_required", "email is required")
		return
	}

	user, err := h.store.GetUserByEmail(c.Request.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			c.JSON(http.StatusAccepted, gin.H{"message": "if the account exists a reset code will be sent"})
			return
		}
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to initiate password reset")
		return
	}
	if !user.EmailVerified || strings.TrimSpace(user.Email) == "" {
		c.JSON(http.StatusAccepted, gin.H{"message": "if the account exists a reset code will be sent"})
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account cannot change password")
		return
	}
	if err := h.enqueuePasswordResetCode(c, user); err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to send password reset code")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "if the account exists a reset code will be sent"})
}

func (h *handler) enqueuePasswordResetCode(c *gin.Context, user *store.User) error {
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
	entry := passwordResetCode{userID: user.ID, email: email, code: code, expiresAt: expiresAt}
	h.passwordResetCodeMu.Lock()
	h.passwordResetCodes[email] = entry
	h.passwordResetCodeMu.Unlock()

	copy := copyFor(requestLocale(c))
	plainBody, htmlBody := renderTransactionalEmail(transactionalEmail{
		Locale:    requestLocale(c),
		Greeting:  fmt.Sprintf(copy.greetingNamed, strings.TrimSpace(user.Name)),
		Intro:     copy.introReset,
		CodeLabel: copy.labelCode,
		Code:      code,
		CodeStyle: codeStyleDigits,
		Expiry:    expiryLine(requestLocale(c), expiresAt, ttl),
		Reassure:  copy.reassureReset,
	})
	if err := h.emailSender.Send(c.Request.Context(), EmailMessage{
		To: []string{email}, Subject: copy.subjectReset, PlainBody: plainBody, HTMLBody: htmlBody,
	}); err != nil {
		h.removePasswordResetCode(email)
		return err
	}
	return nil
}

func (h *handler) lookupPasswordResetCode(email string) (passwordResetCode, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	h.passwordResetCodeMu.RLock()
	entry, ok := h.passwordResetCodes[email]
	h.passwordResetCodeMu.RUnlock()
	if !ok {
		return passwordResetCode{}, false
	}
	if time.Now().After(entry.expiresAt) {
		h.removePasswordResetCode(email)
		return passwordResetCode{}, false
	}
	return entry, true
}

func (h *handler) removePasswordResetCode(email string) {
	h.passwordResetCodeMu.Lock()
	delete(h.passwordResetCodes, strings.ToLower(strings.TrimSpace(email)))
	h.passwordResetCodeMu.Unlock()
}

func (h *handler) recordPasswordResetCodeFailure(email string) time.Time {
	email = strings.ToLower(strings.TrimSpace(email))
	h.passwordResetCodeMu.Lock()
	defer h.passwordResetCodeMu.Unlock()
	entry, ok := h.passwordResetCodes[email]
	if !ok {
		return time.Time{}
	}
	entry.failedAttempts++
	if entry.failedAttempts >= maxMFAVerificationAttempts {
		entry.lockedUntil = time.Now().Add(defaultMFALockoutDuration)
		entry.failedAttempts = 0
	}
	h.passwordResetCodes[email] = entry
	return entry.lockedUntil
}

func (h *handler) confirmPasswordResetCode(c *gin.Context) {
	if hasQueryParameter(c, "email", "code", "password") {
		respondError(c, http.StatusBadRequest, "credentials_in_query", "sensitive credentials must not be sent in the query string")
		return
	}
	var req passwordResetCodeConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	code := strings.TrimSpace(req.Code)
	password := strings.TrimSpace(req.Password)
	if email == "" || code == "" || password == "" {
		respondError(c, http.StatusBadRequest, "invalid_request", "email, code and password are required")
		return
	}
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code must be 6 digits")
		return
	}
	if len(password) < 8 {
		respondError(c, http.StatusBadRequest, "password_too_short", "password must be at least 8 characters")
		return
	}
	entry, ok := h.lookupPasswordResetCode(email)
	if !ok || time.Now().Before(entry.lockedUntil) {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	if entry.code != code {
		if retryAt := h.recordPasswordResetCodeFailure(email); !retryAt.IsZero() {
			respondVerificationLocked(c, retryAt)
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	user, err := h.store.GetUserByID(c.Request.Context(), entry.userID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(user.Email), email) {
		h.removePasswordResetCode(email)
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	if h.isReadOnlyAccount(user) {
		h.removePasswordResetCode(email)
		respondError(c, http.StatusForbidden, "read_only_account", "demo account cannot change password")
		return
	}
	if err := h.replacePassword(c, user, password); err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}
	h.removePasswordResetCode(email)
	c.JSON(http.StatusOK, gin.H{"message": "password reset successful", "user": sanitizeUser(user, nil)})
}

func (h *handler) resetPasswordWithMFA(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	var req mfaPasswordResetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	if !user.MFAEnabled || strings.TrimSpace(user.MFATOTPSecret) == "" {
		respondError(c, http.StatusBadRequest, "mfa_not_enabled", "multi-factor authentication is not enabled")
		return
	}
	if len(strings.TrimSpace(req.Code)) != 6 || len(strings.TrimSpace(req.Password)) < 8 {
		respondError(c, http.StatusBadRequest, "invalid_request", "a 6 digit MFA code and password of at least 8 characters are required")
		return
	}
	valid, err := totp.ValidateCustom(strings.TrimSpace(req.Code), user.MFATOTPSecret, time.Now().UTC(), totp.ValidateOpts{
		Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil || !valid {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_code", "invalid totp code")
		return
	}
	if err := h.replacePassword(c, user, strings.TrimSpace(req.Password)); err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "password reset successful", "user": sanitizeUser(user, nil)})
}

func (h *handler) replacePassword(c *gin.Context, user *store.User, password string) error {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user.PasswordHash = string(hashed)
	user.EmailVerified = true
	return h.store.UpdateUser(c.Request.Context(), user)
}

func (h *handler) selfCancelFreeAccount(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isRootAccount(user) || store.MonthlyQuotaGroup(user) != store.MonthlyFreeQuotaLimitGroup {
		respondError(c, http.StatusForbidden, "free_account_only", "only Free 5GB accounts can self-cancel")
		return
	}
	user.Active = false
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "account_cancel_failed", "failed to cancel account")
		return
	}
	state := &store.AccountQuotaState{AccountUUID: user.ID, ThrottleState: "normal", SuspendState: "active", ProxyAccessState: "paused"}
	if existing, err := h.store.GetAccountQuotaState(c.Request.Context(), user.ID); err == nil && existing != nil {
		state = existing
		state.ProxyAccessState = "paused"
	}
	if err := h.store.UpsertAccountQuotaState(c.Request.Context(), state); err != nil {
		respondError(c, http.StatusInternalServerError, "account_cancel_failed", "failed to pause account access")
		return
	}
	h.removeSession(h.resolveSessionToken(c))
	c.JSON(http.StatusOK, gin.H{"message": "account cancelled", "recoverable": true})
}
