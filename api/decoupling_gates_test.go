package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentserver"
	"account/internal/auth"
	"account/internal/store"
)

func TestDisableLegacyAgentServerRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	st := store.NewMemoryStore()
	tokens := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public-token",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: 24 * time.Hour,
		Store:         st,
	})
	RegisterRoutes(r, WithStore(st), WithTokenService(tokens), WithDisableLegacyAgentServerRoutes(true))

	disabledPaths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/agent-server/v1/users"},
		{http.MethodPost, "/api/agent-server/v1/status"},
		{http.MethodGet, "/api/agent-server/v1/nodes"},
		{http.MethodGet, "/api/agent/nodes"},
		{http.MethodPost, "/api/internal/nodes/heartbeat"},
		{http.MethodPost, "/api/observability/internal/xconnect/identity"},
	}
	for _, tc := range disabledPaths {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404 when disabled", tc.method, tc.path, w.Code)
		}
	}

	for _, path := range []string{"/.well-known/jwks.json", "/api/billing/plans"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, w.Code)
		}
	}
}

func TestDisableLegacyMarketingEmailRoutesPreservesAuthMail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	st := store.NewMemoryStore()
	tokens := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public-token",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: 24 * time.Hour,
		Store:         st,
	})
	RegisterRoutes(r, WithStore(st), WithTokenService(tokens), WithDisableLegacyMarketingEmailRoutes(true))

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/auth/email-preferences"},
		{http.MethodPost, "/api/auth/email-unsubscribe"},
		{http.MethodGet, "/api/auth/admin/email-campaigns"},
		{http.MethodGet, "/api/auth/admin/email-onboarding"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404 when disabled", tc.method, tc.path, w.Code)
		}
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/auth/password/forgot", `{"email":"nobody@example.com"}`},
		{http.MethodPost, "/api/auth/register/send", `{"email":"nobody@example.com"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Fatalf("%s %s returned 404; transactional auth mail routes must remain registered", tc.method, tc.path)
		}
	}
}

func TestAgentUsersCacheTTL(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	u := seedActiveUser(t, st, "Cached User", "cached@example.com", "proxy-cached-1")
	u.EmailVerified = true
	if err := st.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	h := &handler{store: st, agentUsersCacheTTL: time.Minute}
	id := agentserver.Identity{ID: "edge-1", Groups: []string{"resource:free"}}

	c1, _, rev1, err := h.authorizedAgentClientsForIdentityWithPolicy(ctx, id, "")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(c1) != 1 {
		t.Fatalf("len(c1) = %d, want 1", len(c1))
	}

	// Add second user while cache is still hot; cached handler must return rev1.
	u2 := seedActiveUser(t, st, "Second User", "second@example.com", "proxy-cached-2")
	u2.EmailVerified = true
	if err := st.UpdateUser(ctx, u2); err != nil {
		t.Fatal(err)
	}

	c2, _, rev2, err := h.authorizedAgentClientsForIdentityWithPolicy(ctx, id, "")
	if err != nil {
		t.Fatalf("cached call: %v", err)
	}
	if rev2 != rev1 || len(c2) != 1 {
		t.Fatalf("expected cached revision %q (len 1), got %q (len %d)", rev1, rev2, len(c2))
	}

	// Uncached handler sees both users immediately.
	hUncached := &handler{store: st}
	c3, _, rev3, err := hUncached.authorizedAgentClientsForIdentityWithPolicy(ctx, id, "")
	if err != nil {
		t.Fatalf("uncached call: %v", err)
	}
	if rev3 == rev1 || len(c3) != 2 {
		t.Fatalf("expected fresh revision with 2 clients, got %q (len %d)", rev3, len(c3))
	}
}
