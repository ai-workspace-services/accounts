package api

import (
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
)

func TestDisableLegacyOverlayRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	tokenService := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public-token",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: time.Hour,
		Store:         st,
	})

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithTokenService(tokenService), WithDisableLegacyOverlayRoutes(true))

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		if strings.HasPrefix(route.Path, "/api/overlay") || strings.HasPrefix(route.Path, "/api/internal/overlay") {
			t.Fatalf("unexpected legacy overlay route registered when disabled: %s %s", route.Method, route.Path)
		}
		registered[route.Method+" "+route.Path] = true
	}

	requiredCore := []string{
		"GET /.well-known/jwks.json",
		"GET /api/users",
		"GET /api/account/billing/summary",
		"GET /api/billing/plans",
		"GET /api/internal/network/identities",
		"GET /api/auth/network-access",
		"POST /api/task-sessions",
	}
	for _, entry := range requiredCore {
		if !registered[entry] {
			t.Fatalf("expected core accounts route %q to remain registered", entry)
		}
	}
}
