package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/store"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// TestPublicForgotPasswordFlow drives the unauthenticated recovery path:
// /api/auth/password/forgot -> emailed token -> /api/auth/password/forgot/confirm.
// A locked-out user cannot authenticate, so these must live outside the
// session-protected routes.
func TestPublicForgotPasswordFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()

	oldHash, _ := bcrypt.GenerateFromPassword([]byte("originalpass"), bcrypt.DefaultCost)
	user := &store.User{
		Name:          "Forgetful",
		Email:         "forgot@example.com",
		EmailVerified: true,
		PasswordHash:  string(oldHash),
		Active:        true,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	for _, token := range []string{"existing-web-session", "existing-mobile-session"} {
		if err := st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create existing session: %v", err)
		}
	}

	mailer := &testEmailSender{}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailSender(mailer))

	// Request recovery (enumeration-safe 202).
	forgotBody, _ := json.Marshal(map[string]string{"email": "forgot@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot", bytes.NewReader(forgotBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("forgot: expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}

	msg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected reset email")
	}
	token := extractTokenFromMessage(t, msg)
	challenge, err := st.GetPasswordRecoveryChallengeByTokenHash(ctx, passwordResetTokenHash(token))
	if err != nil {
		t.Fatalf("load persisted token challenge: %v", err)
	}
	if challenge.SecretHash == token || challenge.Email != user.Email {
		t.Fatalf("recovery store did not retain only the token digest and account binding: %#v", challenge)
	}
	if _, err := st.GetPasswordRecoveryChallengeByTokenHash(ctx, token); err == nil {
		t.Fatal("raw recovery token unexpectedly matched the persisted token digest")
	}

	// Confirm with new password.
	confirmBody, _ := json.Marshal(map[string]string{"token": token, "password": "brandNewPass9"})
	// A new handler instance must be able to consume the challenge stored by
	// the request handler; challenge state cannot live in a process-local map.
	confirmRouter := gin.New()
	RegisterRoutes(confirmRouter, WithStore(st), WithEmailSender(mailer))
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	confirmRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	reloaded, err := st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(reloaded.PasswordHash), []byte("brandNewPass9")) != nil {
		t.Fatalf("password was not updated to the new value")
	}
	if reloaded.Email != user.Email || !reloaded.EmailVerified || !reloaded.Active || len(reloaded.Groups) != len(user.Groups) {
		t.Fatalf("password recovery unexpectedly changed unrelated account attributes: %#v", reloaded)
	}
	for _, token := range []string{"existing-web-session", "existing-mobile-session"} {
		if _, _, err := st.GetSession(ctx, token); err == nil {
			t.Fatalf("password recovery did not revoke existing session %q", token)
		}
	}

	// The reset token is single-use: a replay must fail.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	confirmRouter.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected reused reset token to be rejected")
	}
}

func TestPublicForgotPasswordCodeIsPersistentOneTimeAndRevokesSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Code Recover", Email: "code-recover@example.com", EmailVerified: true, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.CreateSession(ctx, "code-existing-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	mailer := &testEmailSender{}
	requestRouter := gin.New()
	RegisterRoutes(requestRouter, WithStore(st), WithEmailSender(mailer))
	body, _ := json.Marshal(map[string]string{"email": user.Email})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/send-code", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	requestRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request code: expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	msg, ok := mailer.last()
	if !ok {
		t.Fatal("expected reset code email")
	}
	code := extractVerificationCodeFromMessage(t, msg)
	codeChallenge, err := st.GetLatestPasswordRecoveryCode(ctx, user.Email)
	if err != nil {
		t.Fatalf("load persisted code challenge: %v", err)
	}
	if codeChallenge.SecretHash == code || bcrypt.CompareHashAndPassword([]byte(codeChallenge.SecretHash), []byte(code)) != nil {
		t.Fatal("recovery store did not persist a bcrypt code hash")
	}

	confirmRouter := gin.New()
	RegisterRoutes(confirmRouter, WithStore(st), WithEmailSender(mailer))
	confirmBody, _ := json.Marshal(map[string]string{"email": user.Email, "code": code, "password": "codeResetPass9"})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm-code", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	confirmRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm code: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, err := st.GetSession(ctx, "code-existing-session"); err == nil {
		t.Fatal("successful code recovery did not revoke the existing session")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm-code", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	confirmRouter.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("consumed recovery code was accepted a second time")
	}
}

func TestPublicForgotPasswordTokenExpires(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Expired Recovery", Email: "expired-recovery@example.com", EmailVerified: true, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	mailer := &testEmailSender{}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailSender(mailer), WithPasswordResetTTL(time.Millisecond))
	body, _ := json.Marshal(map[string]string{"email": user.Email})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request token: expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	msg, ok := mailer.last()
	if !ok {
		t.Fatal("expected reset email")
	}
	token := extractTokenFromMessage(t, msg)
	time.Sleep(5 * time.Millisecond)
	confirmBody, _ := json.Marshal(map[string]string{"token": token, "password": "expiredPass9"})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("expired recovery token was accepted")
	}
}

// TestForgotPasswordUnknownEmailIsEnumerationSafe ensures an unknown address
// still returns 202 without leaking whether the account exists.
func TestForgotPasswordUnknownEmailIsEnumerationSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()

	mailer := &testEmailSender{}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailSender(mailer))

	body, _ := json.Marshal(map[string]string{"email": "nobody@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for unknown email, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := mailer.last(); ok {
		t.Fatalf("no email should be sent for an unknown address")
	}
}
