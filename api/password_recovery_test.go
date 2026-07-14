package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

	// Confirm with new password.
	confirmBody, _ := json.Marshal(map[string]string{"token": token, "password": "brandNewPass9"})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
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

	// The reset token is single-use: a replay must fail.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot/confirm", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected reused reset token to be rejected")
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
