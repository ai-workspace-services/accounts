package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"account/internal/auth"
	"account/internal/store"
)

type activeHarness struct {
	router     *gin.Engine
	store      store.Store
	adminToken string
	target     *store.User
	root       *store.User
	password   string
}

func newActiveHarness(t *testing.T) *activeHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()
	// The Active gate lives in auth.RequireActiveUser, which RegisterRoutes
	// only installs when a token service is configured. Without one these
	// tests would pass against a router that never checks Active at all.
	tokenService := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public-token",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: time.Hour,
		Store:         st,
	})
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false), WithTokenService(tokenService))

	const password = "supersecure1"
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	seed := func(id, name, email, role string) *store.User {
		user := &store.User{
			ID:            id,
			Name:          name,
			Email:         email,
			PasswordHash:  string(hashed),
			EmailVerified: true,
			Role:          role,
		}
		if err := st.CreateUser(context.Background(), user); err != nil {
			t.Fatalf("failed to seed %s: %v", name, err)
		}
		return user
	}

	admin := seed("admin-1", "administrator", "admin@example.com", store.RoleAdmin)
	target := seed("user-1", "target user", "target@example.com", store.RoleUser)
	root := seed("root-1", "root", "root@example.com", store.RoleRoot)

	return &activeHarness{
		router:     router,
		store:      st,
		adminToken: loginFor(t, router, admin.Email, password),
		target:     target,
		root:       root,
		password:   password,
	}
}

func loginFor(t *testing.T, router *gin.Engine, email, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"identifier": email, "password": password})
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected login success for %s, got %d: %s", email, rec.Code, rec.Body.String())
	}
	token := decodeResponse(t, rec).Token
	if token == "" {
		t.Fatalf("expected a session token for %s", email)
	}
	return token
}

func (h *activeHarness) post(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *activeHarness) session(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// TestAdminDeactivateThenActivateRoundTrip pins the contract that matters:
// an account can be switched off and back on, and what the user experiences
// in between is exactly the 403 account_suspended that auth.RequireActiveUser
// produces -- login still succeeds, everything behind it does not.
func TestAdminDeactivateThenActivateRoundTrip(t *testing.T) {
	h := newActiveHarness(t)

	userToken := loginFor(t, h.router, h.target.Email, h.password)
	if rec := h.session(t, userToken); rec.Code != http.StatusOK {
		t.Fatalf("expected an active account to resolve its session, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/deactivate", h.adminToken); rec.Code != http.StatusOK {
		t.Fatalf("expected deactivate to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	// Login is deliberately unaffected: it does not consult users.active.
	// This asymmetry is what made the original incident confusing, so pin it.
	freshToken := loginFor(t, h.router, h.target.Email, h.password)
	rec := h.session(t, freshToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a deactivated account, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeResponse(t, rec).Error; got != "account_suspended" {
		t.Fatalf("expected error account_suspended, got %q", got)
	}

	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/activate", h.adminToken); rec.Code != http.StatusOK {
		t.Fatalf("expected activate to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := h.session(t, freshToken); rec.Code != http.StatusOK {
		t.Fatalf("expected the reactivated account to resolve its session, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResumeDoesNotRestoreActive documents the distinction that cost real
// debugging time: resume moves AccountQuotaState.ProxyAccessState (VLESS),
// not users.active. Reaching for resume to fix a locked-out account returns
// 200 and changes nothing the user can feel.
func TestResumeDoesNotRestoreActive(t *testing.T) {
	h := newActiveHarness(t)

	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/deactivate", h.adminToken); rec.Code != http.StatusOK {
		t.Fatalf("expected deactivate to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/resume", h.adminToken); rec.Code != http.StatusOK {
		t.Fatalf("expected resume to report success, got %d: %s", rec.Code, rec.Body.String())
	}

	token := loginFor(t, h.router, h.target.Email, h.password)
	rec := h.session(t, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("resume must not reactivate an account; expected the session to stay 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminCannotDeactivateRoot(t *testing.T) {
	h := newActiveHarness(t)

	rec := h.post(t, "/api/auth/admin/users/"+h.root.ID+"/deactivate", h.adminToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected root to be protected from deactivation, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeResponse(t, rec).Error; got != "root_protected" {
		t.Fatalf("expected error root_protected, got %q", got)
	}

	// And root stays usable afterwards.
	rootToken := loginFor(t, h.router, h.root.Email, h.password)
	if rec := h.session(t, rootToken); rec.Code != http.StatusOK {
		t.Fatalf("expected root to remain active, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestActivateIsIdempotent(t *testing.T) {
	h := newActiveHarness(t)

	for i := 0; i < 2; i++ {
		rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/activate", h.adminToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: expected activate to be idempotent, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	token := loginFor(t, h.router, h.target.Email, h.password)
	if rec := h.session(t, token); rec.Code != http.StatusOK {
		t.Fatalf("expected the account to still resolve its session, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestActivateUnknownUserIsNotFound(t *testing.T) {
	h := newActiveHarness(t)

	rec := h.post(t, "/api/auth/admin/users/does-not-exist/activate", h.adminToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown user, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestActivateRequiresAdmin(t *testing.T) {
	h := newActiveHarness(t)

	userToken := loginFor(t, h.router, h.target.Email, h.password)
	rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/activate", userToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected a plain user to be refused, got %d: %s", rec.Code, rec.Body.String())
	}
}
