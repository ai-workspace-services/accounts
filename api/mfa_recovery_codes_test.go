package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

func TestMFARecoveryCodesAreOneTimeHashedAndRevokeAllSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	secret := "JBSWY3DPEHPK3PXP"
	validUntil := time.Now().UTC().Add(30 * 24 * time.Hour)
	user := &store.User{
		Name: "Protected Account", Email: "protected-mfa@example.invalid", Role: store.RoleAdmin,
		Groups: []string{store.MonthlyPlusQuotaLimitGroup}, Active: true, MFAEnabled: true,
		MFATOTPSecret: secret, MFAConfirmedAt: time.Now().UTC(), SubscriptionValidUntil: &validUntil,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		ID: "paid-subscription", UserID: user.ID, ExternalID: "stripe-subscription", Provider: "stripe", PlanID: "plus", Status: "active",
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	for _, token := range []string{"current-session", "second-device-session"} {
		if err := st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st))

	totpCode, err := totp.GenerateCode(secret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate TOTP: %v", err)
	}
	rotateBody, _ := json.Marshal(map[string]string{"code": totpCode})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/mfa/recovery-codes", bytes.NewReader(rotateBody))
	req.Header.Set("Authorization", "Bearer current-session")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate codes: got %d: %s", rec.Code, rec.Body.String())
	}
	var issued struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil || len(issued.RecoveryCodes) != mfaRecoveryCodeCount {
		t.Fatalf("expected %d one-time codes, got %d (%v)", mfaRecoveryCodeCount, len(issued.RecoveryCodes), err)
	}
	activeCodes, err := st.ListMFARecoveryCodes(ctx, user.ID, time.Now().UTC())
	if err != nil || len(activeCodes) != len(issued.RecoveryCodes) {
		t.Fatalf("load stored code hashes: got %d, error %v", len(activeCodes), err)
	}
	for _, code := range issued.RecoveryCodes {
		normalized := normalizeMFARecoveryCode(code)
		found := false
		for _, stored := range activeCodes {
			if stored.CodeHash == normalized {
				t.Fatalf("plaintext recovery code was persisted")
			}
			if bcrypt.CompareHashAndPassword([]byte(stored.CodeHash), []byte(normalized)) == nil {
				found = true
			}
		}
		if !found {
			t.Fatalf("issued recovery code did not match any stored hash")
		}
	}
	var statusPayload map[string]any
	statusReq := httptest.NewRequest(http.MethodGet, "/api/auth/mfa/recovery-codes", nil)
	statusReq.Header.Set("Authorization", "Bearer current-session")
	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK || strings.Contains(statusRec.Body.String(), activeCodes[0].CodeHash) {
		t.Fatalf("status response leaked hash or failed: %d %s", statusRec.Code, statusRec.Body.String())
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusPayload); err != nil || statusPayload["activeCount"] != float64(mfaRecoveryCodeCount) {
		t.Fatalf("status should expose count only: %#v, %v", statusPayload, err)
	}

	resetBody, _ := json.Marshal(map[string]string{
		"method": "recovery_code", "code": issued.RecoveryCodes[0], "password": "updated-password-9",
	})
	resetReq := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset/mfa", bytes.NewReader(resetBody))
	resetReq.Header.Set("Authorization", "Bearer current-session")
	resetReq.Header.Set("Content-Type", "application/json")
	resetRec := httptest.NewRecorder()
	router.ServeHTTP(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset with recovery code: got %d: %s", resetRec.Code, resetRec.Body.String())
	}
	for _, token := range []string{"current-session", "second-device-session"} {
		if _, _, err := st.GetSession(ctx, token); err == nil {
			t.Fatalf("password reset did not revoke session %q", token)
		}
	}
	updated, err := st.GetUserByID(ctx, user.ID)
	if err != nil || updated.Role != store.RoleAdmin || store.MonthlyQuotaGroup(updated) != store.MonthlyPlusQuotaLimitGroup || updated.SubscriptionValidUntil == nil || !updated.SubscriptionValidUntil.Equal(validUntil) {
		t.Fatalf("password reset changed administrator or paid entitlement: %#v, %v", updated, err)
	}
	subscriptions, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil || len(subscriptions) != 1 || subscriptions[0].Status != "active" {
		t.Fatalf("password reset changed subscription history: %#v, %v", subscriptions, err)
	}
	if err := st.CreateSession(ctx, "replay-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create replay session: %v", err)
	}
	replayReq := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset/mfa", bytes.NewReader(resetBody))
	replayReq.Header.Set("Authorization", "Bearer replay-session")
	replayReq.Header.Set("Content-Type", "application/json")
	replayRec := httptest.NewRecorder()
	router.ServeHTTP(replayRec, replayReq)
	if replayRec.Code == http.StatusOK {
		t.Fatal("consumed recovery code was accepted more than once")
	}
}

func TestMFAPasswordResetKeepsTheExistingTOTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	secret := "JBSWY3DPEHPK3PXP"
	user := &store.User{
		Name: "TOTP Account", Email: "totp-reset@example.invalid", Active: true,
		MFAEnabled: true, MFATOTPSecret: secret, MFAConfirmedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.CreateSession(ctx, "totp-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st))
	code, err := totp.GenerateCode(secret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate TOTP: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"code": code, "password": "totp-password-9"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset/mfa", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer totp-session")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("existing TOTP request contract failed: %d: %s", rec.Code, rec.Body.String())
	}
	if _, _, err := st.GetSession(ctx, "totp-session"); err == nil {
		t.Fatal("TOTP password reset did not revoke the existing session")
	}
}

func TestMFARecoveryCodeFailuresAreLimitedAndArchivedAccountsStayBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	secret := "JBSWY3DPEHPK3PXP"
	user := &store.User{
		Name: "Archived Account", Email: "archived-mfa@example.invalid", Active: true,
		MFAEnabled: true, MFATOTPSecret: secret, MFAConfirmedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	archivedAt := time.Now().UTC()
	archived, _ := st.GetUserByID(ctx, user.ID)
	archived.ArchivedAt = &archivedAt
	if err := st.UpdateUser(ctx, archived); err != nil {
		t.Fatalf("archive user: %v", err)
	}
	if err := st.CreateSession(ctx, "archived-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	wrongHash, _ := bcrypt.GenerateFromPassword([]byte("ABCDE-FGHIJ-KLMNO-PQRST"), bcrypt.DefaultCost)
	if err := st.ReplaceMFARecoveryCodes(ctx, user.ID, []store.MFARecoveryCode{{
		ID: "archived-recovery-code", UserID: user.ID, BatchID: "batch", CodeHash: string(wrongHash),
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}}); err != nil {
		t.Fatalf("store code: %v", err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st))
	resetBody, _ := json.Marshal(map[string]string{"method": "recovery_code", "code": "ABCDE-FGHIJ-KLMNO-PQRST", "password": "updated-password-9"})
	reset := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset/mfa", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer archived-session")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := reset(resetBody); rec.Code != http.StatusForbidden {
		t.Fatalf("archived account password reset should be blocked, got %d: %s", rec.Code, rec.Body.String())
	}
	updated, _ := st.GetUserByID(ctx, user.ID)
	if updated.PasswordHash != "" || updated.ArchivedAt == nil {
		t.Fatalf("archived account was modified: %#v", updated)
	}
	updated.ArchivedAt = nil
	if err := st.UpdateUser(ctx, updated); err != nil {
		t.Fatalf("restore test account: %v", err)
	}
	invalidBody, _ := json.Marshal(map[string]string{"method": "recovery_code", "code": "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ", "password": "updated-password-9"})
	for attempt := 1; attempt <= maxMFAVerificationAttempts; attempt++ {
		rec := reset(invalidBody)
		if attempt < maxMFAVerificationAttempts && rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d should be unauthorized, got %d: %s", attempt, rec.Code, rec.Body.String())
		}
		if attempt == maxMFAVerificationAttempts && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("final bad attempt should be rate limited, got %d: %s", rec.Code, rec.Body.String())
		}
	}
	correctBody, _ := json.Marshal(map[string]string{"method": "recovery_code", "code": "ABCDE-FGHIJ-KLMNO-PQRST", "password": "updated-password-9"})
	if rec := reset(correctBody); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked recovery set should remain rate limited, got %d: %s", rec.Code, rec.Body.String())
	}
}
