package api

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net/http"
	"strings"
	"time"

	"account/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

const (
	mfaRecoveryCodeCount     = 10
	mfaRecoveryCodeTTL       = 365 * 24 * time.Hour
	mfaRecoveryCodeLockout   = 15 * time.Minute
	minMFARecoveryCodeLength = 20
)

type mfaRecoveryCodeRequest struct {
	Code string `json:"code"`
}

func verifyTOTPCode(secret, code string, now time.Time) bool {
	valid, err := totp.ValidateCustom(code, secret, now, totp.ValidateOpts{
		Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && valid
}

func normalizeMFARecoveryCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

func generateMFARecoveryCode() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	parts := []string{encoded[:5], encoded[5:10], encoded[10:15], encoded[15:]}
	return strings.Join(parts, "-"), nil
}

func (h *handler) requireMFARecoveryManagement(c *gin.Context) (*store.User, bool) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return nil, false
	}
	if user.ArchivedAt != nil {
		respondError(c, http.StatusForbidden, "account_archived", "archived accounts must be restored before changing credentials")
		return nil, false
	}
	if !user.MFAEnabled || strings.TrimSpace(user.MFATOTPSecret) == "" {
		respondError(c, http.StatusForbidden, "mfa_not_enabled", "verified MFA is required")
		return nil, false
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "this account cannot change security settings")
		return nil, false
	}
	if !allowAuthProbe(c, h.mfaRecoveryByIP, h.mfaRecoveryByUser, user.ID) {
		return nil, false
	}
	return user, true
}

func (h *handler) requireManagementTOTP(c *gin.Context, user *store.User) bool {
	var req mfaRecoveryCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return false
	}
	if !verifyTOTPCode(user.MFATOTPSecret, strings.TrimSpace(req.Code), time.Now().UTC()) {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_factor", "MFA verification failed")
		return false
	}
	return true
}

func (h *handler) getMFARecoveryCodeStatus(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if user.ArchivedAt != nil {
		respondError(c, http.StatusForbidden, "account_archived", "archived accounts cannot use recovery codes")
		return
	}
	codes, err := h.store.ListMFARecoveryCodes(c.Request.Context(), user.ID, time.Now().UTC())
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "mfa_recovery_unavailable", "recovery code status is unavailable")
		return
	}
	var expiresAt time.Time
	for _, code := range codes {
		if expiresAt.IsZero() || code.ExpiresAt.Before(expiresAt) {
			expiresAt = code.ExpiresAt
		}
	}
	var expiresAtValue any
	if !expiresAt.IsZero() {
		expiresAtValue = expiresAt.UTC()
	}
	c.JSON(http.StatusOK, gin.H{
		"activeCount": len(codes),
		"expiresAt":   expiresAtValue,
	})
}

func (h *handler) rotateMFARecoveryCodes(c *gin.Context) {
	user, ok := h.requireMFARecoveryManagement(c)
	if !ok || !h.requireManagementTOTP(c, user) {
		return
	}
	now := time.Now().UTC()
	expiresAt := now.Add(mfaRecoveryCodeTTL)
	batchID := uuid.NewString()
	codes := make([]store.MFARecoveryCode, 0, mfaRecoveryCodeCount)
	plaintext := make([]string, 0, mfaRecoveryCodeCount)
	for range mfaRecoveryCodeCount {
		code, err := generateMFARecoveryCode()
		if err != nil {
			respondError(c, http.StatusInternalServerError, "mfa_recovery_unavailable", "failed to create recovery codes")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(normalizeMFARecoveryCode(code)), bcrypt.DefaultCost)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "mfa_recovery_unavailable", "failed to create recovery codes")
			return
		}
		codes = append(codes, store.MFARecoveryCode{
			ID: uuid.NewString(), UserID: user.ID, BatchID: batchID, CodeHash: string(hash),
			ExpiresAt: expiresAt, CreatedAt: now,
		})
		plaintext = append(plaintext, code)
	}
	if err := h.store.ReplaceMFARecoveryCodes(c.Request.Context(), user.ID, codes); err != nil {
		respondError(c, http.StatusServiceUnavailable, "mfa_recovery_unavailable", "failed to save recovery codes")
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"recoveryCodes": plaintext,
		"expiresAt":     expiresAt,
		"message":       "recovery codes are shown once; creating a new set revokes the previous set",
	})
}

func (h *handler) revokeMFARecoveryCodes(c *gin.Context) {
	user, ok := h.requireMFARecoveryManagement(c)
	if !ok || !h.requireManagementTOTP(c, user) {
		return
	}
	now := time.Now().UTC()
	codes, err := h.store.ListMFARecoveryCodes(c.Request.Context(), user.ID, now)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "mfa_recovery_unavailable", "failed to load recovery codes")
		return
	}
	if err := h.store.RevokeMFARecoveryCodes(c.Request.Context(), user.ID, now); err != nil {
		if errors.Is(err, store.ErrMFARecoveryCodeInvalid) {
			respondError(c, http.StatusConflict, "mfa_recovery_conflict", "recovery code state changed; retry")
			return
		}
		respondError(c, http.StatusServiceUnavailable, "mfa_recovery_unavailable", "failed to revoke recovery codes")
		return
	}
	c.JSON(http.StatusOK, gin.H{"revokedCount": len(codes)})
}
