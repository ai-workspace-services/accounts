package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/overlay/projection"
	"account/internal/store"
)

var overlayJoinIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

const (
	defaultOverlayJoinTTL       = 15 * time.Minute
	maxOverlayJoinTTL           = 24 * time.Hour
	defaultOverlayEnrollmentTTL = 10 * time.Minute
	maxOverlayEnrollmentTTL     = time.Hour
	maxOverlayJoinBody          = 64 << 10
	overlayEnrollmentContextKey = "overlay_enrollment_session"
)

type OverlayJoinRateLimiter interface {
	Allow(key string, now time.Time) bool
}

type overlayJoinWindow struct {
	start time.Time
	count int
}

type memoryOverlayJoinRateLimiter struct {
	mu      sync.Mutex
	windows map[string]overlayJoinWindow
	limit   int
	window  time.Duration
}

func newMemoryOverlayJoinRateLimiter(limit int, window time.Duration) OverlayJoinRateLimiter {
	return &memoryOverlayJoinRateLimiter{windows: make(map[string]overlayJoinWindow), limit: limit, window: window}
}

func (l *memoryOverlayJoinRateLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) > 4096 {
		for candidate, window := range l.windows {
			if now.Sub(window.start) >= l.window {
				delete(l.windows, candidate)
			}
		}
	}
	state := l.windows[key]
	if state.start.IsZero() || now.Sub(state.start) >= l.window {
		state = overlayJoinWindow{start: now, count: 1}
		l.windows[key] = state
		return true
	}
	if state.count >= l.limit {
		return false
	}
	state.count++
	l.windows[key] = state
	return true
}

type overlayJoinTokenCreateRequest struct {
	NetworkID     string `json:"network_id"`
	DeviceID      string `json:"device_id,omitempty"`
	Platform      string `json:"platform,omitempty"`
	ExpiresIn     int64  `json:"expires_in_seconds,omitempty"`
	RemainingUses int    `json:"remaining_uses,omitempty"`
}

type overlayJoinExchangeRequest struct {
	JoinToken          string `json:"join_token"`
	DeviceID           string `json:"device_id"`
	Name               string `json:"name,omitempty"`
	Platform           string `json:"platform"`
	Hostname           string `json:"hostname,omitempty"`
	WireGuardPublicKey string `json:"wireguard_public_key"`
}

func randomOverlaySecret(prefix string) (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	secret := prefix + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(secret))
	return secret, digest[:], nil
}

func decodeOverlayJoinJSON(c *gin.Context, target any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxOverlayJoinBody))
	if err != nil {
		return errors.New("request payload is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid request payload")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func normalizeOverlayPlatform(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "macos", "mac", "osx", "darwin":
		return "darwin"
	case "windows", "win32", "win64":
		return "windows"
	case "linux":
		return "linux"
	case "ios":
		return "ios"
	case "android":
		return "android"
	default:
		return ""
	}
}

func normalizeOverlayJoinID(value string) string {
	normalized := sanitizeOverlayID(value)
	if !overlayJoinIDPattern.MatchString(normalized) {
		return ""
	}
	return normalized
}

func (h *handler) overlayControllerURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv("OVERLAY_CONTROLLER_URL"))
	if raw == "" {
		return "", errors.New("OVERLAY_CONTROLLER_URL is not configured")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("OVERLAY_CONTROLLER_URL must not contain userinfo, query, or fragment components")
	}
	insecureLocalhost := parsed.Scheme == "http" &&
		(parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1") &&
		strings.EqualFold(strings.TrimSpace(os.Getenv("OVERLAY_ALLOW_INSECURE_LOCALHOST")), "true")
	if parsed.Scheme != "https" && !insecureLocalhost {
		return "", errors.New("OVERLAY_CONTROLLER_URL must be HTTPS")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (h *handler) createOverlayJoinToken(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}
	var request overlayJoinTokenCreateRequest
	if err := decodeOverlayJoinJSON(c, &request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	controller, err := h.overlayControllerURL()
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "overlay_controller_unavailable", "overlay controller URL is not configured")
		return
	}
	ttl := defaultOverlayJoinTTL
	if request.ExpiresIn != 0 {
		ttl = time.Duration(request.ExpiresIn) * time.Second
	}
	if ttl <= 0 || ttl > maxOverlayJoinTTL {
		respondError(c, http.StatusBadRequest, "invalid_expiry", "expires_in_seconds must be between 1 and 86400")
		return
	}
	uses := request.RemainingUses
	if uses == 0 {
		uses = 1
	}
	if uses != 1 {
		respondError(c, http.StatusBadRequest, "invalid_remaining_uses", "join tokens are one-time; remaining_uses may only be 0 or 1")
		return
	}
	networkID := normalizeOverlayNetworkID(request.NetworkID)
	if !overlayJoinIDPattern.MatchString(networkID) {
		respondError(c, http.StatusBadRequest, "invalid_network_id", "network_id is invalid")
		return
	}
	deviceID := ""
	if strings.TrimSpace(request.DeviceID) != "" {
		deviceID = normalizeOverlayJoinID(request.DeviceID)
		if deviceID == "" {
			respondError(c, http.StatusBadRequest, "invalid_device_id", "device_id is invalid")
			return
		}
	}
	platform := ""
	if strings.TrimSpace(request.Platform) != "" {
		platform = normalizeOverlayPlatform(request.Platform)
		if platform == "" {
			respondError(c, http.StatusBadRequest, "invalid_platform", "platform is unsupported")
			return
		}
	}
	secret, digest, err := randomOverlaySecret("xjt_")
	if err != nil {
		respondError(c, http.StatusInternalServerError, "join_token_failed", "failed to create join token")
		return
	}
	now := time.Now().UTC()
	token := &store.OverlayJoinToken{
		ID: "join_" + strings.ReplaceAll(uuid.NewString(), "-", ""), TokenHash: digest,
		UserID: user.ID, NetworkID: networkID, DeviceID: deviceID,
		Platform: platform, RemainingUses: uses, ExpiresAt: now.Add(ttl),
	}
	audit := &store.AuditLog{Action: store.AuditActionOverlayJoinCreate, ActorUUID: user.ID, Details: map[string]any{
		"target_uuid": user.ID, "join_token_id": token.ID, "network_id": token.NetworkID,
		"device_id": token.DeviceID, "platform": token.Platform, "remaining_uses": uses, "expires_at": token.ExpiresAt,
	}}
	if err := h.store.CreateOverlayJoinToken(c.Request.Context(), token, audit); err != nil {
		respondError(c, http.StatusInternalServerError, "join_token_failed", "failed to create join token")
		return
	}
	joinURL := &url.URL{Scheme: "xconnect", Host: "join", Path: "/" + secret}
	query := joinURL.Query()
	query.Set("controller", controller)
	joinURL.RawQuery = query.Encode()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"join_token": gin.H{
		"id": token.ID, "join_uri": joinURL.String(), "network_id": token.NetworkID,
		"device_id": token.DeviceID, "platform": token.Platform, "remaining_uses": token.RemainingUses,
		"expires_at": token.ExpiresAt,
	}})
}

func (h *handler) revokeOverlayJoinToken(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}
	tokenID := strings.TrimSpace(c.Param("id"))
	if tokenID == "" {
		respondError(c, http.StatusBadRequest, "join_token_id_required", "join token id is required")
		return
	}
	now := time.Now().UTC()
	audit := &store.AuditLog{Action: store.AuditActionOverlayJoinRevoke, ActorUUID: user.ID, Details: map[string]any{
		"target_uuid": user.ID, "join_token_id": tokenID,
	}}
	if err := h.store.RevokeOverlayJoinToken(c.Request.Context(), user.ID, tokenID, now, audit); err != nil {
		if errors.Is(err, store.ErrOverlayJoinTokenNotFound) {
			respondError(c, http.StatusNotFound, "join_token_not_found", "join token was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "join_token_revoke_failed", "failed to revoke join token")
		return
	}
	c.Status(http.StatusNoContent)
}

func overlayEnrollmentTTL() time.Duration {
	ttl := defaultOverlayEnrollmentTTL
	if raw := strings.TrimSpace(os.Getenv("OVERLAY_ENROLLMENT_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 && parsed <= maxOverlayEnrollmentTTL {
			ttl = parsed
		}
	}
	return ttl
}

func (h *handler) exchangeOverlayJoinToken(c *gin.Context) {
	if !requireSecureOverlayControl(c) {
		return
	}
	if strings.TrimSpace(c.GetHeader("Content-Type")) != "application/json" {
		respondError(c, http.StatusUnsupportedMediaType, "content_type_required", "Content-Type must be application/json")
		return
	}
	var request overlayJoinExchangeRequest
	if err := decodeOverlayJoinJSON(c, &request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	joinSecret := strings.TrimSpace(request.JoinToken)
	joinDigest := sha256.Sum256([]byte(joinSecret))
	if h.overlayJoinRateLimiter != nil {
		now := time.Now().UTC()
		if !h.overlayJoinRateLimiter.Allow("ip:"+c.ClientIP(), now) || !h.overlayJoinRateLimiter.Allow("token:"+hex.EncodeToString(joinDigest[:8]), now) {
			respondError(c, http.StatusTooManyRequests, "join_rate_limited", "too many join attempts")
			return
		}
	}
	if !strings.HasPrefix(joinSecret, "xjt_") || len(joinSecret) < 40 {
		respondError(c, http.StatusUnauthorized, "invalid_join_token", "join token is invalid or unavailable")
		return
	}
	deviceID := normalizeOverlayJoinID(request.DeviceID)
	platform := normalizeOverlayPlatform(request.Platform)
	publicKey := strings.TrimSpace(request.WireGuardPublicKey)
	if deviceID == "" || platform == "" || !isWireGuardKey(publicKey) {
		respondError(c, http.StatusBadRequest, "invalid_device", "device_id, supported platform, and a WireGuard public key are required")
		return
	}
	if len(request.Name) > 255 || len(request.Hostname) > 255 {
		respondError(c, http.StatusBadRequest, "invalid_device", "device name and hostname must be at most 255 characters")
		return
	}
	enrollmentSecret, enrollmentDigest, err := randomOverlaySecret("xenr_")
	if err != nil {
		respondError(c, http.StatusInternalServerError, "join_exchange_failed", "failed to exchange join token")
		return
	}
	deviceCredentialSecret, deviceCredential, err := generateOverlayDeviceCredential()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "join_exchange_failed", "failed to exchange join token")
		return
	}
	now := time.Now().UTC()
	deviceCredential.IssuedAt = now
	deviceCredential.CreatedAt = now
	deviceCredential.ExpiresAt = now.Add(defaultOverlayDeviceCredentialTTL)
	exchange := &store.OverlayJoinExchange{
		JoinTokenHash: joinDigest[:],
		Enrollment: store.OverlayEnrollmentSession{
			ID: "enr_" + strings.ReplaceAll(uuid.NewString(), "-", ""), TokenHash: enrollmentDigest,
			CreatedAt: now, ExpiresAt: now.Add(overlayEnrollmentTTL()),
		},
		DeviceCredential: *deviceCredential,
		Device: store.OverlayDevice{
			ID: deviceID, Name: strings.TrimSpace(request.Name), Platform: platform,
			Hostname: strings.TrimSpace(request.Hostname), WireGuardPublicKey: publicKey,
		},
		AddressPrefix:    envOrDefault("OVERLAY_WIREGUARD_PREFIX", defaultOverlayCIDRPrefix),
		AddressStartHost: defaultOverlayDeviceStartHost, AddressEndHost: defaultOverlayDeviceEndHost,
	}
	if exchange.Device.Name == "" {
		exchange.Device.Name = deviceID
	}
	audit := &store.AuditLog{Action: store.AuditActionOverlayJoinExchange, Details: map[string]any{
		"device_id": deviceID, "platform": platform,
	}}
	if err := h.store.ExchangeOverlayJoinToken(c.Request.Context(), exchange, audit); err != nil {
		switch {
		case errors.Is(err, store.ErrOverlayJoinConstraint):
			respondError(c, http.StatusForbidden, "join_constraint_mismatch", "join token does not permit this device")
		case errors.Is(err, store.ErrOverlayJoinDeviceConflict):
			respondError(c, http.StatusConflict, "device_registration_conflict", "device is already registered with different identity material")
		case errors.Is(err, store.ErrOverlayJoinTokenNotFound), errors.Is(err, store.ErrOverlayJoinTokenExpired), errors.Is(err, store.ErrOverlayJoinTokenRevoked), errors.Is(err, store.ErrOverlayJoinTokenExhausted), errors.Is(err, store.ErrOverlayJoinReplay):
			// Expired, revoked, exhausted, unknown, and replayed secrets share a
			// response so the public endpoint cannot be used as a token oracle.
			respondError(c, http.StatusUnauthorized, "invalid_join_token", "join token is invalid or unavailable")
		default:
			respondError(c, http.StatusInternalServerError, "join_exchange_failed", "failed to exchange join token")
		}
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"enrollment_token": enrollmentSecret, "token_type": "Bearer", "expires_at": exchange.Enrollment.ExpiresAt,
		"scope":             []string{"overlay:config:read", "overlay:config:ack", "overlay:device:revoke"},
		"device_credential": gin.H{"credential_id": exchange.DeviceCredential.ID, "credential": deviceCredentialSecret, "token_type": "Device", "issued_at": exchange.DeviceCredential.IssuedAt, "expires_at": exchange.DeviceCredential.ExpiresAt, "scope": exchange.DeviceCredential.Scopes},
		"device":            overlayJoinDevicePayload(&exchange.Device), "network": overlayNetworkPayload(exchange.Device.NetworkID),
		"signing_keys": h.overlayProjectionPublicKeys(),
	})
}

// overlayJoinDevicePayload is frozen to the strict join-exchange v1 schema.
// Lifecycle-only fields are available from the authenticated device APIs and
// are intentionally not added to this no-store bootstrap response.
func overlayJoinDevicePayload(device *store.OverlayDevice) gin.H {
	return gin.H{
		"id": device.ID, "user_id": device.UserID, "network_id": device.NetworkID,
		"name": device.Name, "platform": device.Platform, "hostname": device.Hostname,
		"wireguard_public_key": device.WireGuardPublicKey, "wireguard_address": device.WireGuardAddress,
		"created_at": device.CreatedAt, "updated_at": device.UpdatedAt, "last_seen_at": device.LastSeenAt,
	}
}

func (h *handler) overlayProjectionPublicKeys() []projection.PublicSigningKey {
	if h.overlayProjection == nil {
		return []projection.PublicSigningKey{}
	}
	return h.overlayProjection.PublicSigningKeys()
}

func enrollmentBearer(c *gin.Context) string {
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

func (h *handler) requireOverlayEnrollment(c *gin.Context, requiredScope string) (*store.OverlayEnrollmentSession, bool) {
	secret := enrollmentBearer(c)
	if !strings.HasPrefix(secret, "xenr_") {
		respondError(c, http.StatusUnauthorized, "invalid_enrollment", "enrollment token is invalid or expired")
		return nil, false
	}
	digest := sha256.Sum256([]byte(secret))
	session, err := h.store.GetOverlayEnrollmentSession(c.Request.Context(), digest[:], time.Now().UTC())
	if err != nil {
		session, err = h.store.GetOverlayDeviceSession(c.Request.Context(), digest[:], time.Now().UTC())
	}
	if err != nil {
		respondError(c, http.StatusUnauthorized, "invalid_enrollment", "enrollment token is invalid or expired")
		return nil, false
	}
	hasScope := false
	for _, scope := range session.Scopes {
		if scope == requiredScope {
			hasScope = true
			break
		}
	}
	if !hasScope {
		respondError(c, http.StatusForbidden, "enrollment_scope_denied", "enrollment token does not permit this operation")
		return nil, false
	}
	device, err := h.store.GetOverlayDevice(c.Request.Context(), session.UserID, session.DeviceID)
	if err != nil || (device.Status != "" && device.Status != store.OverlayDeviceActive) || device.NetworkID != session.NetworkID || (session.Platform != "" && device.Platform != session.Platform) || (session.WireGuardPublicKey != "" && device.WireGuardPublicKey != session.WireGuardPublicKey) {
		respondError(c, http.StatusUnauthorized, "invalid_enrollment", "enrollment token is invalid or expired")
		return nil, false
	}
	c.Set(overlayEnrollmentContextKey, session)
	return session, true
}

func bindEnrollmentQuery(c *gin.Context, session *store.OverlayEnrollmentSession) bool {
	query := c.Request.URL.Query()
	if value := sanitizeOverlayID(query.Get("device_id")); value != "" && value != session.DeviceID {
		respondError(c, http.StatusForbidden, "enrollment_scope_denied", "enrollment token is bound to another device")
		return false
	}
	if value := strings.TrimSpace(query.Get("network_id")); value != "" && normalizeOverlayNetworkID(value) != session.NetworkID {
		respondError(c, http.StatusForbidden, "enrollment_scope_denied", "enrollment token is bound to another network")
		return false
	}
	query.Set("device_id", session.DeviceID)
	query.Set("network_id", session.NetworkID)
	c.Request.URL.RawQuery = query.Encode()
	return true
}

func bindEnrollmentAckBody(c *gin.Context, session *store.OverlayEnrollmentSession) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxSignedConfigAckBody))
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_ack", "ack payload is too large")
		return false
	}
	var envelope struct {
		DeviceID  string `json:"device_id"`
		NetworkID string `json:"network_id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_ack", "invalid acknowledgement payload")
		return false
	}
	if sanitizeOverlayID(envelope.DeviceID) != session.DeviceID || (envelope.NetworkID != "" && normalizeOverlayNetworkID(envelope.NetworkID) != session.NetworkID) {
		respondError(c, http.StatusForbidden, "enrollment_scope_denied", "enrollment token is bound to another device or network")
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	return true
}

func (h *handler) enrollmentOverlayConfig(c *gin.Context) {
	session, ok := h.requireOverlayEnrollment(c, "overlay:config:read")
	if ok && bindEnrollmentQuery(c, session) {
		h.overlayConfig(c)
	}
}

func (h *handler) enrollmentOverlaySignedConfig(c *gin.Context) {
	session, ok := h.requireOverlayEnrollment(c, "overlay:config:read")
	if ok && bindEnrollmentQuery(c, session) {
		h.overlaySignedConfig(c)
	}
}

func (h *handler) enrollmentOverlayConfigAck(c *gin.Context) {
	session, ok := h.requireOverlayEnrollment(c, "overlay:config:ack")
	if ok && bindEnrollmentAckBody(c, session) {
		h.overlayConfigAck(c)
	}
}

func (h *handler) enrollmentOverlaySignedConfigAck(c *gin.Context) {
	session, ok := h.requireOverlayEnrollment(c, "overlay:config:ack")
	if ok && bindEnrollmentAckBody(c, session) {
		h.overlaySignedConfigAck(c)
	}
}

func overlayEnrollmentFromContext(c *gin.Context) (*store.OverlayEnrollmentSession, bool) {
	value, exists := c.Get(overlayEnrollmentContextKey)
	if !exists {
		return nil, false
	}
	session, ok := value.(*store.OverlayEnrollmentSession)
	return session, ok && session != nil
}

func activeEnrollmentUser(ctx context.Context, st store.Store, session *store.OverlayEnrollmentSession) (*store.User, error) {
	user, err := st.GetUserByID(ctx, session.UserID)
	if err != nil || !user.Active {
		return nil, errors.New("enrollment account is unavailable")
	}
	return user, nil
}
