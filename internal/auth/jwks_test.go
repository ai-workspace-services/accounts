package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestTokenServiceEdDSAAndJWKSWithHS256Compatibility(t *testing.T) {
	pubCurr, privCurr, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubPrev, privPrev, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	svc := NewTokenService(TokenConfig{
		PublicToken:              "pub",
		AccessSecret:             "hs256-transition-secret",
		RefreshSecret:            "refresh-secret",
		AccessExpiry:             time.Hour,
		RefreshExpiry:            24 * time.Hour,
		SigningKeyID:             "accounts-jwt-202611",
		SigningPrivateKey:        privCurr,
		PreviousSigningKeyID:     "accounts-jwt-202610",
		PreviousSigningPublicKey: pubPrev,
	})

	pair, err := svc.GenerateTokenPair("user-123", "alice@example.test", []string{"admin"})
	if err != nil {
		t.Fatalf("GenerateTokenPair failed: %v", err)
	}
	claims, err := svc.ValidateAccessToken(pair.AccessToken)
	if err != nil {
		t.Fatalf("ValidateAccessToken(EdDSA current) failed: %v", err)
	}
	if claims.UserID != "user-123" || claims.Email != "alice@example.test" || len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Fatalf("unexpected EdDSA claims: %#v", claims)
	}

	// Token signed by previous Ed25519 key remains valid during rotation overlap.
	prevSvc := NewTokenService(TokenConfig{
		AccessSecret:      "hs256-transition-secret",
		RefreshSecret:     "refresh-secret",
		AccessExpiry:      time.Hour,
		RefreshExpiry:     24 * time.Hour,
		SigningKeyID:      "accounts-jwt-202610",
		SigningPrivateKey: privPrev,
	})
	prevPair, err := prevSvc.GenerateTokenPair("user-456", "bob@example.test", []string{"user"})
	if err != nil {
		t.Fatal(err)
	}
	prevClaims, err := svc.ValidateAccessToken(prevPair.AccessToken)
	if err != nil || prevClaims.UserID != "user-456" {
		t.Fatalf("expected previous EdDSA key overlap verification to succeed, claims=%#v err=%v", prevClaims, err)
	}

	// Legacy HS256 token continues validating during transition.
	hsSvc := NewTokenService(TokenConfig{
		AccessSecret:  "hs256-transition-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: 24 * time.Hour,
	})
	hsPair, err := hsSvc.GenerateTokenPair("user-789", "carol@example.test", []string{"user"})
	if err != nil {
		t.Fatal(err)
	}
	hsClaims, err := svc.ValidateAccessToken(hsPair.AccessToken)
	if err != nil || hsClaims.UserID != "user-789" {
		t.Fatalf("expected HS256 backward-compat verification to succeed, claims=%#v err=%v", hsClaims, err)
	}

	// JWKS outputs RFC 8037 OKP format for both current and previous keys.
	jwks := svc.JWKS()
	if len(jwks.Keys) != 2 {
		t.Fatalf("expected 2 JWK entries, got %#v", jwks)
	}
	if jwks.Keys[0].KeyType != "OKP" || jwks.Keys[0].Curve != "Ed25519" || jwks.Keys[0].Algorithm != "EdDSA" || jwks.Keys[0].KeyID != "accounts-jwt-202611" || jwks.Keys[0].X != base64.RawURLEncoding.EncodeToString(pubCurr) {
		t.Fatalf("unexpected current JWK: %#v", jwks.Keys[0])
	}
	if jwks.Keys[1].KeyID != "accounts-jwt-202610" || jwks.Keys[1].X != base64.RawURLEncoding.EncodeToString(pubPrev) {
		t.Fatalf("unexpected previous JWK: %#v", jwks.Keys[1])
	}
	raw, err := json.Marshal(jwks)
	if err != nil || len(raw) == 0 {
		t.Fatalf("marshal JWKS failed: %v", err)
	}
}

func TestLoadJWTSigningConfigFromEnv(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prevPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("JWT_SIGNING_KEY_ID", "kid-seed")
	t.Setenv("JWT_SIGNING_PRIVATE_KEY", base64.RawStdEncoding.EncodeToString(priv.Seed()))
	t.Setenv("JWT_PREVIOUS_SIGNING_KEY_ID", "kid-prev")
	t.Setenv("JWT_PREVIOUS_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(prevPub))

	cfg, err := LoadJWTSigningConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadJWTSigningConfigFromEnv(seed) failed: %v", err)
	}
	if cfg.SigningKeyID != "kid-seed" || cfg.SigningPrivateKey.Public().(ed25519.PublicKey) == nil || len(cfg.PreviousSigningPublicKey) != ed25519.PublicKeySize {
		t.Fatalf("unexpected loaded config: %#v", cfg)
	}
	if base64.RawURLEncoding.EncodeToString(cfg.SigningPrivateKey.Public().(ed25519.PublicKey)) != base64.RawURLEncoding.EncodeToString(pub) {
		t.Fatal("derived public key from 32-byte seed mismatch")
	}

	// Full 64-byte private key is also accepted.
	t.Setenv("JWT_SIGNING_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv))
	cfg2, err := LoadJWTSigningConfigFromEnv()
	if err != nil || len(cfg2.SigningPrivateKey) != ed25519.PrivateKeySize {
		t.Fatalf("LoadJWTSigningConfigFromEnv(64B) failed: %v", err)
	}
}
