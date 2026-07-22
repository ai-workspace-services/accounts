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
)

func TestComputeServiceReadiness(t *testing.T) {
	cases := []struct {
		name     string
		user     *store.User
		ready    bool
		nextStep string
	}{
		{
			name:     "fresh oauth user: nothing done",
			user:     &store.User{EmailVerified: false},
			ready:    false,
			nextStep: readinessReqEmail,
		},
		{
			name:     "verified only: needs password next",
			user:     &store.User{EmailVerified: true},
			ready:    false,
			nextStep: readinessReqPassword,
		},
		{
			name:     "verified + password: needs mfa next",
			user:     &store.User{EmailVerified: true, PasswordHash: "hash"},
			ready:    false,
			nextStep: readinessReqMFA,
		},
		{
			name:  "all three: ready",
			user:  &store.User{EmailVerified: true, PasswordHash: "hash", MFAEnabled: true},
			ready: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeServiceReadiness(tc.user)
			if got.Ready != tc.ready {
				t.Fatalf("ready: got %v want %v", got.Ready, tc.ready)
			}
			if got.NextStep != tc.nextStep {
				t.Fatalf("nextStep: got %q want %q", got.NextStep, tc.nextStep)
			}
			if len(got.Requirements) != 3 {
				t.Fatalf("expected 3 requirements, got %d", len(got.Requirements))
			}
		})
	}
}

func TestServiceReadinessEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()

	user := &store.User{Name: "OAuth User", Email: "svc@example.com", Active: true, EmailVerified: false}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := "readiness-session"
	if err := st.CreateSession(ctx, token, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/account/service-readiness", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Readiness serviceReadinessState `json:"readiness"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Readiness.Ready {
		t.Fatalf("fresh oauth user should not be ready")
	}
	if payload.Readiness.NextStep != readinessReqEmail {
		t.Fatalf("nextStep: got %q want %q", payload.Readiness.NextStep, readinessReqEmail)
	}
}

func TestSetPasswordFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()

	// Verified OAuth user with no password yet.
	user := &store.User{Name: "Pwd User", Email: "pwd@example.com", Active: true, EmailVerified: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := "setpw-session"
	if err := st.CreateSession(ctx, token, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	set := func(pw string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"password": pw})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/password/set", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Too short → 400.
	if rec := set("short"); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: got %d body=%s", rec.Code, rec.Body.String())
	}

	// Valid → 200 + password now set.
	rec := set("sup3rsecret")
	if rec.Code != http.StatusOK {
		t.Fatalf("set password: got %d body=%s", rec.Code, rec.Body.String())
	}
	reloaded, err := st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.PasswordHash == "" {
		t.Fatalf("expected password hash to be set")
	}

	// Second attempt → 409 (already set; use reset to rotate).
	if rec := set("anotherpass"); rec.Code != http.StatusConflict {
		t.Fatalf("second set: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetPasswordRequiresVerifiedEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()

	user := &store.User{Name: "Unverified", Email: "unv@example.com", Active: true, EmailVerified: false}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := "unv-session"
	if err := st.CreateSession(ctx, token, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	body, _ := json.Marshal(map[string]string{"password": "sup3rsecret"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/set", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified set password: got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRequireAdvancedServiceReadinessGate exercises the reusable gate directly
// (no advanced-service routes exist yet): an unready user gets a 403 with the
// intro flag and readiness state; a ready user passes.
func TestRequireAdvancedServiceReadinessGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{store: st}

	mint := func(u *store.User) string {
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		tok := "gate-" + u.Email
		if err := st.CreateSession(ctx, tok, u.ID, time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
		return tok
	}

	// Unready user → 403 advanced_service_locked + intro.
	unready := mint(&store.User{Name: "U", Email: "u@example.com", Active: true, EmailVerified: true})
	{
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
		c.Request.Header.Set("Authorization", "Bearer "+unready)
		if _, ok := h.requireAdvancedServiceReadiness(c); ok {
			t.Fatalf("expected gate to block unready user")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("gate status: got %d", rec.Code)
		}
		var body struct {
			Error string `json:"error"`
			Intro bool   `json:"intro"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != "advanced_service_locked" || !body.Intro {
			t.Fatalf("unexpected gate body: %s", rec.Body.String())
		}
	}

	// Ready user → passes.
	ready := mint(&store.User{Name: "R", Email: "r@example.com", Active: true, EmailVerified: true, PasswordHash: "hash", MFAEnabled: true})
	{
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
		c.Request.Header.Set("Authorization", "Bearer "+ready)
		if _, ok := h.requireAdvancedServiceReadiness(c); !ok {
			t.Fatalf("expected gate to pass ready user, body=%s", rec.Body.String())
		}
	}
}
