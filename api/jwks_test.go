package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
)

func TestWellKnownJWKSAndETag304(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokenSvc := auth.NewTokenService(auth.TokenConfig{
		AccessSecret:      "fallback",
		RefreshSecret:     "refresh",
		AccessExpiry:      time.Hour,
		RefreshExpiry:     time.Hour,
		SigningKeyID:      "accounts-jwt-202611",
		SigningPrivateKey: priv,
	})
	router := gin.New()
	RegisterRoutes(router, WithStore(store.NewMemoryStore()), WithTokenService(tokenSvc))

	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /.well-known/jwks.json, got %d: %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected non-empty ETag header on /.well-known/jwks.json")
	}
	var body auth.JWKS
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Keys) != 1 || body.Keys[0].KeyID != "accounts-jwt-202611" {
		t.Fatalf("unexpected JWKS response: %#v", body)
	}

	req304 := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	router.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified for matching ETag, got %d", rec304.Code)
	}
}
