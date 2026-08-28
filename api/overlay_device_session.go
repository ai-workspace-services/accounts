package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/store"
)

const defaultOverlayDeviceCredentialTTL = 30 * 24 * time.Hour
const maxOverlayDeviceCredentialTTLAPI = 31 * 24 * time.Hour
const maxOverlayDeviceControlBody = 64 << 10

var overlayDeviceCredentialIDPattern = regexp.MustCompile(`^xdcid_[0-9a-f]{32}$`)
var overlayDeviceCredentialPattern = regexp.MustCompile(`^xdc_([0-9a-f]{32})\.([A-Za-z0-9_-]{43})$`)
var overlaySHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type overlayDeviceSessionRequest struct {
	ClientNonce string `json:"client_nonce"`
}

type overlayDeviceCredentialRotateRequest struct {
	NewCredentialID     string `json:"new_credential_id"`
	NewCredentialSHA256 string `json:"new_credential_sha256"`
}

func generateOverlayDeviceCredential() (string, *store.OverlayDeviceCredential, error) {
	idBytes, secretBytes := make([]byte, 16), make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, err
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return "", nil, err
	}
	idSegment := hex.EncodeToString(idBytes)
	secretSegment := base64.RawURLEncoding.EncodeToString(secretBytes)
	raw := "xdc_" + idSegment + "." + secretSegment
	verifier := sha256.Sum256([]byte(raw))
	now := time.Now().UTC()
	return raw, &store.OverlayDeviceCredential{
		ID: "xdcid_" + idSegment, Verifier: verifier[:], Status: store.OverlayDeviceCredentialActive,
		Scopes:   []string{"overlay:session:mint", "overlay:credential:rotate", "overlay:device:revoke"},
		IssuedAt: now, ExpiresAt: now.Add(defaultOverlayDeviceCredentialTTL), CreatedAt: now,
	}, nil
}

func requireSecureOverlayControl(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
	secure := c.Request.TLS != nil
	if !secure && strings.EqualFold(strings.TrimSpace(os.Getenv("OVERLAY_TRUST_FORWARDED_HTTPS")), "true") && strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https") {
		secure = true
	}
	if !secure {
		respondError(c, http.StatusBadRequest, "https_required", "device control endpoints require HTTPS")
		return false
	}
	return true
}

func strictOverlayDeviceJSON(c *gin.Context, target any) ([]byte, bool) {
	if strings.TrimSpace(c.GetHeader("Content-Type")) != "application/json" {
		respondError(c, http.StatusUnsupportedMediaType, "content_type_required", "Content-Type must be application/json")
		return nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxOverlayDeviceControlBody))
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "request payload is too large")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return nil, false
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respondError(c, http.StatusBadRequest, "invalid_request", "request must contain one JSON object")
		return nil, false
	}
	return raw, true
}

func canonicalUUID(value string) (string, bool) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || parsed.String() != strings.TrimSpace(value) {
		return "", false
	}
	return parsed.String(), true
}

func deviceAuthorization(c *gin.Context) (string, []byte, bool) {
	values := c.Request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", nil, false
	}
	header := values[0]
	separator := strings.IndexByte(header, ' ')
	if separator <= 0 || strings.IndexByte(header[separator+1:], ' ') >= 0 || !strings.EqualFold(header[:separator], "Device") {
		return "", nil, false
	}
	raw := header[separator+1:]
	matches := overlayDeviceCredentialPattern.FindStringSubmatch(raw)
	if len(matches) != 3 {
		return "", nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(matches[2])
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != matches[2] {
		return "", nil, false
	}
	verifier := sha256.Sum256([]byte(raw))
	return "xdcid_" + matches[1], verifier[:], true
}

func canonicalRequestHash(c *gin.Context, value any) (string, bool) {
	canonical, err := json.Marshal(value)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "request is not canonicalizable")
		return "", false
	}
	digest := sha256.Sum256(canonical)
	want := "sha256-" + hex.EncodeToString(digest[:])
	if c.GetHeader("Idempotency-Key") != want {
		respondError(c, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must equal sha256 of the canonical request body")
		return "", false
	}
	return hex.EncodeToString(digest[:]), true
}

func rejectDeviceAuthorization(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
	respondError(c, http.StatusUnauthorized, "invalid_device_credential", "device credential is invalid or unavailable")
}

func (h *handler) mintOverlayDeviceSession(c *gin.Context) {
	if !requireSecureOverlayControl(c) {
		return
	}
	credentialID, verifier, ok := deviceAuthorization(c)
	if !ok {
		rejectDeviceAuthorization(c)
		return
	}
	credential, err := h.store.AuthenticateOverlayDeviceCredential(c.Request.Context(), credentialID, verifier, time.Now().UTC())
	if err != nil {
		rejectDeviceAuthorization(c)
		return
	}
	signingKeys := h.overlayProjectionPublicKeys()
	if len(signingKeys) == 0 {
		c.Header("Cache-Control", "no-store")
		c.Header("Vary", "Authorization")
		respondError(c, http.StatusServiceUnavailable, "signing_keys_unavailable", "overlay signing keys are unavailable")
		return
	}
	var request overlayDeviceSessionRequest
	if _, ok = strictOverlayDeviceJSON(c, &request); !ok {
		return
	}
	nonce, ok := canonicalUUID(request.ClientNonce)
	if !ok {
		respondError(c, http.StatusBadRequest, "invalid_client_nonce", "client_nonce must be a canonical UUID")
		return
	}
	secret, digest, err := randomOverlaySecret("xenr_")
	if err != nil {
		respondError(c, http.StatusInternalServerError, "device_session_failed", "failed to mint device session")
		return
	}
	now := time.Now().UTC()
	session := &store.OverlayEnrollmentSession{ID: "dsess_" + strings.ReplaceAll(uuid.NewString(), "-", ""), TokenHash: digest, Scopes: []string{"overlay:config:read", "overlay:config:ack"}, ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now}
	audit := &store.AuditLog{Action: store.AuditActionOverlayDeviceSessionMint, ActorUUID: credential.UserID, Details: map[string]any{}}
	if err = h.store.MintOverlayDeviceSession(c.Request.Context(), credential.ID, session, now, audit); err != nil {
		rejectDeviceAuthorization(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
	c.JSON(http.StatusOK, gin.H{"client_nonce": nonce, "enrollment_token": secret, "token_type": "Bearer", "issued_at": now, "expires_at": session.ExpiresAt, "scope": []string{"overlay:config:read", "overlay:config:ack"}, "device_id": credential.DeviceID, "network_id": credential.NetworkID, "signing_keys": signingKeys})
}

func (h *handler) rotateOverlayDeviceCredential(c *gin.Context) {
	if !requireSecureOverlayControl(c) {
		return
	}
	credentialID, verifier, ok := deviceAuthorization(c)
	if !ok {
		rejectDeviceAuthorization(c)
		return
	}
	current, err := h.store.AuthenticateOverlayDeviceCredential(c.Request.Context(), credentialID, verifier, time.Now().UTC())
	if err != nil {
		rejectDeviceAuthorization(c)
		return
	}
	var request overlayDeviceCredentialRotateRequest
	if _, ok = strictOverlayDeviceJSON(c, &request); !ok {
		return
	}
	if !overlayDeviceCredentialIDPattern.MatchString(request.NewCredentialID) || !overlaySHA256Pattern.MatchString(request.NewCredentialSHA256) || request.NewCredentialID == current.ID {
		respondError(c, http.StatusBadRequest, "invalid_device_credential_rotation", "successor credential id and verifier are invalid")
		return
	}
	requestHash, ok := canonicalRequestHash(c, request)
	if !ok {
		return
	}
	verifierBytes, _ := hex.DecodeString(request.NewCredentialSHA256)
	now := time.Now().UTC()
	successor := &store.OverlayDeviceCredential{ID: request.NewCredentialID, Verifier: verifierBytes, Scopes: []string{"overlay:session:mint", "overlay:credential:rotate", "overlay:device:revoke"}, IssuedAt: now, ExpiresAt: now.Add(defaultOverlayDeviceCredentialTTL)}
	audit := &store.AuditLog{Action: store.AuditActionOverlayDeviceCredentialRotate, ActorUUID: current.UserID, Details: map[string]any{"credential_id": successor.ID, "replaces_credential_id": current.ID, "device_id": current.DeviceID}}
	stored, _, err := h.store.RotateOverlayDeviceCredential(c.Request.Context(), current.ID, successor, requestHash, now, audit)
	if err != nil {
		if errors.Is(err, store.ErrOverlayDeviceCredentialIdempotency) || errors.Is(err, store.ErrOverlayDeviceCredentialConflict) {
			respondError(c, http.StatusConflict, "device_credential_conflict", "device credential rotation conflicts with persisted state")
			return
		}
		rejectDeviceAuthorization(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
	c.JSON(http.StatusOK, gin.H{"credential_id": stored.ID, "replaces_credential_id": stored.ReplacesCredentialID, "token_type": "Device", "issued_at": stored.IssuedAt, "expires_at": stored.ExpiresAt, "scope": stored.Scopes})
}

func (h *handler) revokeOverlayDeviceWithCredential(c *gin.Context) {
	if !requireSecureOverlayControl(c) {
		return
	}
	credentialID, verifier, ok := deviceAuthorization(c)
	if !ok {
		rejectDeviceAuthorization(c)
		return
	}
	var request overlayDeviceSessionRequest
	if _, ok = strictOverlayDeviceJSON(c, &request); !ok {
		return
	}
	nonce, ok := canonicalUUID(request.ClientNonce)
	if !ok {
		respondError(c, http.StatusBadRequest, "invalid_client_nonce", "client_nonce must be a canonical UUID")
		return
	}
	request.ClientNonce = nonce
	requestHash, ok := canonicalRequestHash(c, request)
	if !ok {
		return
	}
	audit := &store.AuditLog{Action: store.AuditActionOverlayDeviceRevoke, Details: map[string]any{"credential_id": credentialID, "source": "device_credential_leave"}}
	receipt, duplicate, err := h.store.RevokeOverlayDeviceWithCredential(c.Request.Context(), credentialID, verifier, requestHash, nonce, time.Now().UTC(), audit)
	if err != nil {
		if errors.Is(err, store.ErrOverlayDeviceCredentialIdempotency) {
			respondError(c, http.StatusConflict, "device_revoke_idempotency_conflict", "device revoke receipt is bound to another canonical request")
			return
		}
		rejectDeviceAuthorization(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
	c.JSON(http.StatusAccepted, gin.H{"revoked": true, "duplicate": duplicate, "device": overlayDevicePayload(&receipt.Device), "policy_reconcile_pending": true})
}
