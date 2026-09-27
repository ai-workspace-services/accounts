package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	Method   string `json:"method"`
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
	if !allowAuthProbe(c, h.passwordRecoveryByIP, h.passwordRecoveryByEmail, email) {
		return
	}

	user, err := h.store.GetUserByEmail(c.Request.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondPasswordRecoveryAccepted(c)
			return
		}
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to initiate password reset")
		return
	}
	if !user.EmailVerified || strings.TrimSpace(user.Email) == "" {
		respondPasswordRecoveryAccepted(c)
		return
	}
	if h.isReadOnlyAccount(user) {
		respondPasswordRecoveryAccepted(c)
		return
	}
	if err := h.enqueuePasswordResetCode(c, user); err != nil {
		if !errors.Is(err, store.ErrPasswordRecoveryCooldown) {
			// Keep the public response identical to unknown-address and cooldown
			// responses; logging the delivery failure is operational only.
			slog.Error("password recovery code could not be delivered", "error", err)
		}
		respondPasswordRecoveryAccepted(c)
		return
	}
	respondPasswordRecoveryAccepted(c)
}

func respondPasswordRecoveryAccepted(c *gin.Context) {
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
	codeHash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	challenge := &store.PasswordRecoveryChallenge{
		ID: uuid.NewString(), UserID: user.ID, Email: email, Kind: "code",
		SecretHash: string(codeHash), ExpiresAt: expiresAt, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.CreatePasswordRecoveryCodeChallenge(c.Request.Context(), challenge, passwordRecoveryCodeCooldown); err != nil {
		return err
	}

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
		_ = h.store.InvalidatePasswordRecoveryChallenge(c.Request.Context(), challenge.ID, time.Now().UTC())
		return err
	}
	return nil
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
	if !allowAuthProbe(c, h.passwordRecoveryConfirmByIP, h.passwordRecoveryConfirmByEmail, email) {
		return
	}
	challenge, err := h.store.GetLatestPasswordRecoveryCode(c.Request.Context(), email)
	if errors.Is(err, store.ErrPasswordRecoveryInvalid) {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to load password reset challenge")
		return
	}
	if challenge != nil && !challenge.ExpiresAt.After(time.Now()) {
		respondError(c, http.StatusGone, "code_expired", "verification code has expired")
		return
	}
	if challenge == nil || time.Now().Before(challenge.LockedUntil) {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(challenge.SecretHash), []byte(code)) != nil {
		retryAt, failureErr := h.store.RecordPasswordRecoveryFailure(c.Request.Context(), challenge.ID, time.Now().UTC(), maxMFAVerificationAttempts, defaultMFALockoutDuration)
		if errors.Is(failureErr, store.ErrPasswordRecoveryInvalid) {
			respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
			return
		}
		if failureErr != nil {
			respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to update password reset attempt state")
			return
		}
		if !retryAt.IsZero() {
			respondError(c, http.StatusTooManyRequests, "rate_limited", "too many attempts, please try again later")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	user, err := h.store.GetUserByID(c.Request.Context(), challenge.UserID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(user.Email), email) {
		_ = h.store.InvalidatePasswordRecoveryChallenge(c.Request.Context(), challenge.ID, time.Now().UTC())
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}
	if h.isReadOnlyAccount(user) {
		_ = h.store.InvalidatePasswordRecoveryChallenge(c.Request.Context(), challenge.ID, time.Now().UTC())
		respondError(c, http.StatusForbidden, "read_only_account", "demo account cannot change password")
		return
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}
	if err := h.store.CompletePasswordRecovery(c.Request.Context(), challenge.ID, string(hashed), time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrPasswordRecoveryInvalid) {
			respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
			return
		}
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "password reset successful", "user": sanitizeUser(user, nil)})
}

func (h *handler) resetPasswordWithMFA(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if user.ArchivedAt != nil {
		respondError(c, http.StatusForbidden, "account_archived", "archived accounts must be restored before changing credentials")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "this account cannot change its password")
		return
	}
	if !allowAuthProbe(c, h.mfaRecoveryByIP, h.mfaRecoveryByUser, user.ID) {
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
	password := strings.TrimSpace(req.Password)
	if len(password) < 8 {
		respondError(c, http.StatusBadRequest, "password_too_short", "password must be at least 8 characters")
		return
	}
	method := strings.ToLower(strings.TrimSpace(req.Method))
	if method == "" {
		method = "totp" // Preserve the existing endpoint contract.
	}
	code := strings.TrimSpace(req.Code)
	var recoveryCodeID string
	switch method {
	case "totp":
		if len(code) != 6 || strings.Trim(code, "0123456789") != "" || !verifyTOTPCode(user.MFATOTPSecret, code, time.Now().UTC()) {
			respondError(c, http.StatusUnauthorized, "invalid_mfa_factor", "MFA verification failed")
			return
		}
	case "recovery_code":
		if len(normalizeMFARecoveryCode(code)) < minMFARecoveryCodeLength {
			respondError(c, http.StatusUnauthorized, "invalid_mfa_factor", "MFA verification failed")
			return
		}
		codes, err := h.store.ListMFARecoveryCodes(c.Request.Context(), user.ID, time.Now().UTC())
		if err != nil {
			respondError(c, http.StatusInternalServerError, "mfa_recovery_unavailable", "failed to verify recovery code")
			return
		}
		for _, recoveryCode := range codes {
			if time.Now().Before(recoveryCode.LockedUntil) {
				respondError(c, http.StatusTooManyRequests, "rate_limited", "too many attempts, please try again later")
				return
			}
		}
		for _, recoveryCode := range codes {
			if bcrypt.CompareHashAndPassword([]byte(recoveryCode.CodeHash), []byte(normalizeMFARecoveryCode(code))) == nil {
				recoveryCodeID = recoveryCode.ID
				break
			}
		}
		if recoveryCodeID == "" {
			lockedUntil, err := h.store.RecordMFARecoveryCodeFailure(c.Request.Context(), user.ID, time.Now().UTC(), maxMFAVerificationAttempts, mfaRecoveryCodeLockout)
			if err != nil && !errors.Is(err, store.ErrMFARecoveryCodeInvalid) {
				respondError(c, http.StatusInternalServerError, "mfa_recovery_unavailable", "failed to record recovery attempt")
				return
			}
			if !lockedUntil.IsZero() {
				respondError(c, http.StatusTooManyRequests, "rate_limited", "too many attempts, please try again later")
				return
			}
			respondError(c, http.StatusUnauthorized, "invalid_mfa_factor", "MFA verification failed")
			return
		}
	default:
		respondError(c, http.StatusBadRequest, "invalid_mfa_method", "unsupported MFA method")
		return
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}
	if err := h.store.CompleteMFAPasswordReset(c.Request.Context(), user.ID, recoveryCodeID, string(hashed), time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrMFARecoveryCodeInvalid) {
			respondError(c, http.StatusUnauthorized, "invalid_mfa_factor", "MFA verification failed")
			return
		}
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "password reset successful", "sessionsRevoked": true})
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
