package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"account/internal/store"
)

// TokenPair represents a pair of Public and Access tokens
type TokenPair struct {
	PublicToken  string `json:"public_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Claims represents JWT access token claims
type Claims struct {
	UserID string   `json:"user_id"`
	Email  string   `json:"email"`
	Roles  []string `json:"roles"`
	MFA    bool     `json:"mfa_verified"`
	jwt.RegisteredClaims
}

// JWK represents an RFC 8037 Ed25519 JSON Web Key entry.
type JWK struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	KeyID     string `json:"kid"`
	X         string `json:"x"`
}

// JWKS represents a JSON Web Key Set payload returned by /.well-known/jwks.json.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWTSigningConfig holds optional Ed25519 asymmetric JWT signing settings loaded from environment variables.
type JWTSigningConfig struct {
	SigningKeyID             string
	SigningPrivateKey        ed25519.PrivateKey
	PreviousSigningKeyID     string
	PreviousSigningPublicKey ed25519.PublicKey
}

// TokenService handles token generation and validation
type TokenService struct {
	publicToken              string
	refreshSecret            string
	accessSecret             string
	accessExpiry             time.Duration
	refreshExpiry            time.Duration
	signingKeyID             string
	signingPrivateKey        ed25519.PrivateKey
	previousSigningKeyID     string
	previousSigningPublicKey ed25519.PublicKey
	store                    store.Store
}

// TokenConfig holds configuration for token service
type TokenConfig struct {
	PublicToken              string
	RefreshSecret            string
	AccessSecret             string
	AccessExpiry             time.Duration
	RefreshExpiry            time.Duration
	SigningKeyID             string
	SigningPrivateKey        ed25519.PrivateKey
	PreviousSigningKeyID     string
	PreviousSigningPublicKey ed25519.PublicKey
	Store                    store.Store
}

// NewTokenService creates a new TokenService instance
func NewTokenService(config TokenConfig) *TokenService {
	keyID := strings.TrimSpace(config.SigningKeyID)
	var privKey ed25519.PrivateKey
	if len(config.SigningPrivateKey) == ed25519.PrivateKeySize {
		privKey = append(ed25519.PrivateKey(nil), config.SigningPrivateKey...)
		if keyID == "" {
			keyID = "accounts-jwt-current"
		}
	}
	prevKeyID := strings.TrimSpace(config.PreviousSigningKeyID)
	var prevPub ed25519.PublicKey
	if len(config.PreviousSigningPublicKey) == ed25519.PublicKeySize {
		prevPub = append(ed25519.PublicKey(nil), config.PreviousSigningPublicKey...)
		if prevKeyID == "" {
			prevKeyID = "accounts-jwt-previous"
		}
	}
	return &TokenService{
		publicToken:              config.PublicToken,
		refreshSecret:            config.RefreshSecret,
		accessSecret:             config.AccessSecret,
		accessExpiry:             config.AccessExpiry,
		refreshExpiry:            config.RefreshExpiry,
		signingKeyID:             keyID,
		signingPrivateKey:        privKey,
		previousSigningKeyID:     prevKeyID,
		previousSigningPublicKey: prevPub,
		store:                    config.Store,
	}
}

func decodeBase64Flexible(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(raw)
	}
	return decoded, err
}

// LoadJWTSigningConfigFromEnv loads optional Ed25519 JWT signing and overlap verification keys from environment variables.
func LoadJWTSigningConfigFromEnv() (JWTSigningConfig, error) {
	cfg := JWTSigningConfig{
		SigningKeyID:         strings.TrimSpace(os.Getenv("JWT_SIGNING_KEY_ID")),
		PreviousSigningKeyID: strings.TrimSpace(os.Getenv("JWT_PREVIOUS_SIGNING_KEY_ID")),
	}
	if privText := strings.TrimSpace(os.Getenv("JWT_SIGNING_PRIVATE_KEY")); privText != "" {
		decoded, err := decodeBase64Flexible(privText)
		if err != nil {
			return JWTSigningConfig{}, errors.New("JWT_SIGNING_PRIVATE_KEY must be base64 Ed25519 private key or 32-byte seed")
		}
		switch len(decoded) {
		case ed25519.PrivateKeySize:
			cfg.SigningPrivateKey = ed25519.PrivateKey(decoded)
		case ed25519.SeedSize:
			cfg.SigningPrivateKey = ed25519.NewKeyFromSeed(decoded)
		default:
			return JWTSigningConfig{}, errors.New("JWT_SIGNING_PRIVATE_KEY must be 64-byte Ed25519 private key or 32-byte seed")
		}
		if cfg.SigningKeyID == "" {
			cfg.SigningKeyID = "accounts-jwt-current"
		}
	}
	if prevPubText := strings.TrimSpace(os.Getenv("JWT_PREVIOUS_SIGNING_PUBLIC_KEY")); prevPubText != "" {
		decoded, err := decodeBase64Flexible(prevPubText)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return JWTSigningConfig{}, errors.New("JWT_PREVIOUS_SIGNING_PUBLIC_KEY must be 32-byte base64 Ed25519 public key")
		}
		cfg.PreviousSigningPublicKey = ed25519.PublicKey(decoded)
		if cfg.PreviousSigningKeyID == "" {
			cfg.PreviousSigningKeyID = "accounts-jwt-previous"
		}
	}
	return cfg, nil
}

// SetStore sets the store for the token service.
func (s *TokenService) SetStore(st store.Store) {
	s.store = st
}

// ValidatePublicToken validates the public token
func (s *TokenService) ValidatePublicToken(publicToken string) bool {
	return publicToken == s.publicToken
}

// GeneratePublicToken returns the configured public token.
func (s *TokenService) GeneratePublicToken(userID, email string, roles []string) string {
	return s.publicToken
}

func (s *TokenService) signAccessClaims(claims Claims) (string, error) {
	if len(s.signingPrivateKey) == ed25519.PrivateKeySize {
		accessToken := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		accessToken.Header["kid"] = s.signingKeyID
		return accessToken.SignedString(s.signingPrivateKey)
	}
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return accessToken.SignedString([]byte(s.accessSecret))
}

func (s *TokenService) eddsaVerificationKey(kid string) (ed25519.PublicKey, error) {
	kid = strings.TrimSpace(kid)
	if len(s.signingPrivateKey) == ed25519.PrivateKeySize {
		currentPub := s.signingPrivateKey.Public().(ed25519.PublicKey)
		if kid == "" || kid == s.signingKeyID {
			return currentPub, nil
		}
	}
	if len(s.previousSigningPublicKey) == ed25519.PublicKeySize && kid != "" && kid == s.previousSigningKeyID {
		return s.previousSigningPublicKey, nil
	}
	return nil, fmt.Errorf("unknown EdDSA signing key kid %q", kid)
}

// JWKS returns the public Ed25519 verification keys in RFC 8037 OKP format.
func (s *TokenService) JWKS() JWKS {
	keys := make([]JWK, 0, 2)
	var currentPub ed25519.PublicKey
	if s != nil && len(s.signingPrivateKey) == ed25519.PrivateKeySize {
		currentPub = s.signingPrivateKey.Public().(ed25519.PublicKey)
		keys = append(keys, JWK{
			KeyType:   "OKP",
			Curve:     "Ed25519",
			Algorithm: "EdDSA",
			Use:       "sig",
			KeyID:     s.signingKeyID,
			X:         base64.RawURLEncoding.EncodeToString(currentPub),
		})
	}
	if s != nil && len(s.previousSigningPublicKey) == ed25519.PublicKeySize && !bytes.Equal(s.previousSigningPublicKey, currentPub) {
		keys = append(keys, JWK{
			KeyType:   "OKP",
			Curve:     "Ed25519",
			Algorithm: "EdDSA",
			Use:       "sig",
			KeyID:     s.previousSigningKeyID,
			X:         base64.RawURLEncoding.EncodeToString(s.previousSigningPublicKey),
		})
	}
	return JWKS{Keys: keys}
}

// GenerateTokenPair generates a new token pair
func (s *TokenService) GenerateTokenPair(userID, email string, roles []string) (*TokenPair, error) {
	// Generate refresh token (JWT)
	refreshClaims := jwt.RegisteredClaims{
		Subject:   userID,
		Audience:  []string{"xcontrol-refresh"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.refreshExpiry)),
		NotBefore: jwt.NewNumericDate(time.Now()),
		Issuer:    "xcontrol-account",
	}

	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshTokenString, err := refreshToken.SignedString([]byte(s.refreshSecret))
	if err != nil {
		return nil, fmt.Errorf("failed to sign refresh token: %w", err)
	}

	// Generate access token (JWT)
	claims := Claims{
		UserID: userID,
		Email:  email,
		Roles:  roles,
		MFA:    true, // Assume MFA is verified for now
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Audience:  []string{"xcontrol-access"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.accessExpiry)),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "xcontrol-account",
		},
	}

	accessTokenString, err := s.signAccessClaims(claims)
	if err != nil {
		return nil, fmt.Errorf("failed to sign access token: %w", err)
	}

	return &TokenPair{
		PublicToken:  s.publicToken,
		AccessToken:  accessTokenString,
		RefreshToken: refreshTokenString,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.accessExpiry.Seconds()),
	}, nil
}

// ValidateAccessToken validates and parses an access token
func (s *TokenService) ValidateAccessToken(accessToken string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(accessToken, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		switch token.Method.(type) {
		case *jwt.SigningMethodEd25519:
			kid, _ := token.Header["kid"].(string)
			return s.eddsaVerificationKey(kid)
		case *jwt.SigningMethodHMAC:
			if s.accessSecret == "" {
				return nil, fmt.Errorf("HMAC verification key not configured")
			}
			return []byte(s.accessSecret), nil
		default:
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to parse access token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid access token")
	}

	return claims, nil
}

// RefreshAccessToken generates a new access token using refresh token
func (s *TokenService) RefreshAccessToken(refreshToken string) (string, error) {
	token, err := jwt.ParseWithClaims(refreshToken, &jwt.RegisteredClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(s.refreshSecret), nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to parse refresh token: %w", err)
	}

	claims, ok := token.Claims.(*jwt.RegisteredClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid refresh token")
	}

	// Verify issuer and audience
	if claims.Issuer != "xcontrol-account" {
		return "", fmt.Errorf("invalid token issuer")
	}

	if !contains(claims.Audience, "xcontrol-refresh") {
		return "", fmt.Errorf("invalid token audience")
	}

	// Generate new access token
	newClaims := Claims{
		UserID: claims.Subject,
		Email:  "", // Will be populated from user store
		Roles:  []string{"user"},
		MFA:    true,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   claims.Subject,
			Audience:  []string{"xcontrol-access"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.accessExpiry)),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "xcontrol-account",
		},
	}

	accessTokenString, err := s.signAccessClaims(newClaims)
	if err != nil {
		return "", fmt.Errorf("failed to sign access token: %w", err)
	}

	return accessTokenString, nil
}

// GetAccessTokenExpiry returns the access token expiry duration
func (s *TokenService) GetAccessTokenExpiry() time.Duration {
	return s.accessExpiry
}

// Helper function to check if a slice contains a string
func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}
