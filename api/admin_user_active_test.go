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
	"github.com/pquerna/otp/totp"
)

type activeHarness struct {
	tokenService *auth.TokenService
	router       *gin.Engine
	store        store.Store
	adminToken   string
	target       *store.User
	root         *store.User
	password     string
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
		tokenService: tokenService,
		router:       router,
		store:        st,
		adminToken:   loginFor(t, router, admin.Email, password),
		target:       target,
		root:         root,
		password:     password,
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

// Account deactivation blocks both new login and existing sessions.
func TestAdminDeactivateThenActivateRoundTrip(t *testing.T) {
	h := newActiveHarness(t)

	userToken := loginFor(t, h.router, h.target.Email, h.password)
	if rec := h.session(t, userToken); rec.Code != http.StatusOK {
		t.Fatalf("expected an active account to resolve its session, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/deactivate", h.adminToken); rec.Code != http.StatusOK {
		t.Fatalf("expected deactivate to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	assertLoginSuspended(t, h)
	rec := h.session(t, userToken)
	if rec.Code != http.StatusForbidden || decodeResponse(t, rec).Error != "account_suspended" {
		t.Fatalf("expected suspended existing session, got %d", rec.Code)
	}

	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/activate", h.adminToken); rec.Code != http.StatusOK {
		t.Fatalf("expected activate to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := h.session(t, userToken); rec.Code != http.StatusOK {
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

	assertLoginSuspended(t, h)
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

func assertLoginSuspended(t *testing.T, h *activeHarness) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": h.target.Email, "password": h.password})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || decodeResponse(t, rec).Error != "account_suspended" {
		t.Fatalf("disabled account login must fail, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPauseDoesNotBlockLoginOrChangeAccount(t *testing.T) {
	h := newActiveHarness(t)
	before, _ := h.store.GetUserByID(context.Background(), h.target.ID)
	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/pause", h.adminToken); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	token := loginFor(t, h.router, h.target.Email, h.password)
	if rec := h.session(t, token); rec.Code != http.StatusOK {
		t.Fatalf("paused user must still log in: %d", rec.Code)
	}
	after, _ := h.store.GetUserByID(context.Background(), h.target.ID)
	if after.Active != before.Active || after.EmailVerified != before.EmailVerified {
		t.Fatal("pause modified account status")
	}
	state, err := h.store.GetAccountQuotaState(context.Background(), h.target.ID)
	if err != nil || state.ProxyAccessState != "paused" {
		t.Fatalf("expected paused proxy state: %v", err)
	}
	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/resume", h.adminToken); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	state, _ = h.store.GetAccountQuotaState(context.Background(), h.target.ID)
	if state.ProxyAccessState != "active" {
		t.Fatal("proxy not resumed")
	}
}

func TestMFATicketCannotBypassDeactivation(t *testing.T) {
	h := newActiveHarness(t)
	user, _ := h.store.GetUserByID(context.Background(), h.target.ID)
	user.MFAEnabled = true
	user.MFATOTPSecret = "JBSWY3DPEHPK3PXP"
	if err := h.store.UpdateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"email": user.Email, "password": h.password})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var challenge map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	ticket, _ := challenge["mfaTicket"].(string)
	if ticket == "" {
		t.Fatal("no MFA ticket issued")
	}
	if rec := h.post(t, "/api/auth/admin/users/"+user.ID+"/deactivate", h.adminToken); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	code, err := totp.GenerateCode(user.MFATOTPSecret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]string{"mfa_ticket": ticket, "code": code})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/mfa/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != 403 || !bytes.Contains(rec.Body.Bytes(), []byte("account_suspended")) {
		t.Fatalf("MFA bypassed deactivation: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshTokenHonorsDisabledAccount(t *testing.T) {
	h := newActiveHarness(t)
	pair, err := h.tokenService.GenerateTokenPair(h.target.ID, h.target.Email, []string{store.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	refresh := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"refresh_token": pair.RefreshToken})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/token/refresh", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, req)
		return rec
	}
	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/pause", h.adminToken); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := refresh(); rec.Code != 200 {
		t.Fatalf("pause must preserve token refresh: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.post(t, "/api/auth/admin/users/"+h.target.ID+"/deactivate", h.adminToken); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := refresh(); rec.Code != 403 || !bytes.Contains(rec.Body.Bytes(), []byte("account_suspended")) {
		t.Fatalf("disabled account refreshed tokens: %d %s", rec.Code, rec.Body.String())
	}
}
