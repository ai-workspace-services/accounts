package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/overlay/gatewayprojection"
	"account/internal/overlay/projection"
	"account/internal/store"
)

var gatewayNodeSecretPattern = regexp.MustCompile(`^xgn_[A-Za-z0-9_-]{43}$`)

const (
	gatewayMediaType         = "application/vnd.xconnect.gateway.v1+json"
	maxGatewayBody           = 64 << 10
	defaultNodeCredentialTTL = 24 * time.Hour
	maxNodeCredentialTTL     = 30 * 24 * time.Hour
)

type gatewayHeartbeatRequest struct {
	NodeID             string `json:"node_id"`
	AgentVersion       string `json:"agent_version"`
	Mode               string `json:"mode"`
	ProxyCore          string `json:"proxy_core"`
	ObservedGeneration uint64 `json:"observed_generation"`
	AppliedGeneration  uint64 `json:"applied_generation"`
}

type gatewayDiffSummary struct {
	Status          string `json:"status"`
	Equal           bool   `json:"equal"`
	ProjectedPeers  int    `json:"projected_peers"`
	CurrentPeers    int    `json:"current_peers"`
	MissingPeers    int    `json:"missing_peers"`
	UnexpectedPeers int    `json:"unexpected_peers"`
	RouteMismatches int    `json:"route_mismatches"`
}

type gatewayApplyResultRequest struct {
	NodeID             string             `json:"node_id"`
	SnapshotID         string             `json:"snapshot_id"`
	ObservedGeneration uint64             `json:"observed_generation"`
	AppliedGeneration  uint64             `json:"applied_generation"`
	RuntimeApplied     bool               `json:"runtime_applied"`
	Result             string             `json:"result"`
	Diff               gatewayDiffSummary `json:"diff"`
}

func newOverlayGatewayProjectionServiceFromEnvironment(st store.Store, signing *projection.Service) (*gatewayprojection.Service, error) {
	maxRemoval := 20.0
	if raw := strings.TrimSpace(os.Getenv("OVERLAY_GATEWAY_MAX_PEER_REMOVAL_PERCENT")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, errors.New("OVERLAY_GATEWAY_MAX_PEER_REMOVAL_PERCENT must be numeric")
		}
		maxRemoval = value
	}
	lifetime := 30 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("OVERLAY_GATEWAY_SNAPSHOT_TTL")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return nil, errors.New("OVERLAY_GATEWAY_SNAPSHOT_TTL must be positive")
		}
		lifetime = value
	}
	return gatewayprojection.NewService(st, signing, time.Now, gatewayprojection.Config{Lifetime: lifetime, RenewalInterval: lifetime / 2, InterfaceName: envOrDefault("OVERLAY_WIREGUARD_INTERFACE", defaultOverlayInterface), WireGuardListenPort: envIntOrDefault("OVERLAY_GATEWAY_WIREGUARD_LISTEN_PORT", 51820), RelayListenHost: envOrDefault("OVERLAY_GATEWAY_RELAY_LISTEN_HOST", "0.0.0.0"), PersistentKeepaliveSeconds: envIntOrDefault("OVERLAY_WIREGUARD_KEEPALIVE", defaultOverlayKeepalive), AllowEmptyPeers: strings.EqualFold(strings.TrimSpace(os.Getenv("OVERLAY_GATEWAY_ALLOW_EMPTY_PEERS")), "true"), MaxPeerRemovalPercent: maxRemoval})
}

func requireJSONContentType(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		respondError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	return true
}

func strictGatewayJSON(c *gin.Context, target any) bool {
	if !requireJSONContentType(c) {
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxGatewayBody))
	if err != nil {
		respondError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "gateway payload is too large")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "gateway payload is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respondError(c, http.StatusBadRequest, "invalid_request", "gateway payload must contain one JSON object")
		return false
	}
	return true
}

func gatewayNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization")
}

func (h *handler) requireGatewayNode(c *gin.Context) (*store.OverlayNodeCredential, bool) {
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		respondError(c, http.StatusUnauthorized, "invalid_node_credential", "node credential is invalid or expired")
		return nil, false
	}
	secret := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if !gatewayNodeSecretPattern.MatchString(secret) {
		respondError(c, http.StatusUnauthorized, "invalid_node_credential", "node credential is invalid or expired")
		return nil, false
	}
	digest := sha256.Sum256([]byte(secret))
	credential, err := h.store.AuthenticateOverlayNodeCredential(c.Request.Context(), digest[:], time.Now().UTC())
	if err != nil {
		respondError(c, http.StatusUnauthorized, "invalid_node_credential", "node credential is invalid or expired")
		return nil, false
	}
	return credential, true
}

func (h *handler) createOverlayNodeCredential(c *gin.Context) {
	var request struct {
		ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
	}
	if !strictGatewayJSON(c, &request) {
		return
	}
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if _, err := h.store.GetOverlayNode(c.Request.Context(), nodeID); err != nil {
		respondError(c, http.StatusNotFound, "overlay_node_not_found", "overlay node was not bootstrapped")
		return
	}
	ttl := defaultNodeCredentialTTL
	if request.ExpiresInSeconds != 0 {
		ttl = time.Duration(request.ExpiresInSeconds) * time.Second
	}
	if ttl <= 0 || ttl > maxNodeCredentialTTL {
		respondError(c, http.StatusBadRequest, "invalid_expiry", "expires_in_seconds must be between 1 and 2592000")
		return
	}
	secret, digest, err := randomOverlaySecret("xgn_")
	if err != nil {
		respondError(c, http.StatusInternalServerError, "credential_create_failed", "failed to create node credential")
		return
	}
	now := time.Now().UTC()
	credential := &store.OverlayNodeCredential{ID: "nodecred_" + strings.ReplaceAll(uuid.NewString(), "-", ""), NodeID: nodeID, TokenHash: digest, ExpiresAt: now.Add(ttl)}
	audit := &store.AuditLog{Action: store.AuditActionOverlayNodeCredentialCreate, Details: map[string]any{"node_id": nodeID, "credential_id": credential.ID, "expires_at": credential.ExpiresAt}}
	if err := h.store.CreateOverlayNodeCredential(c.Request.Context(), credential, audit); err != nil {
		respondError(c, http.StatusInternalServerError, "credential_create_failed", "failed to create node credential")
		return
	}
	gatewayNoStore(c)
	c.JSON(http.StatusCreated, gin.H{"credential": gin.H{"id": credential.ID, "node_id": nodeID, "bearer_token": secret, "expires_at": credential.ExpiresAt}})
}

func (h *handler) revokeOverlayNodeCredential(c *gin.Context) {
	nodeID, credentialID := strings.TrimSpace(c.Param("node_id")), strings.TrimSpace(c.Param("credential_id"))
	now := time.Now().UTC()
	audit := &store.AuditLog{Action: store.AuditActionOverlayNodeCredentialRevoke, Details: map[string]any{"node_id": nodeID, "credential_id": credentialID}}
	if err := h.store.RevokeOverlayNodeCredential(c.Request.Context(), nodeID, credentialID, now, audit); err != nil {
		if errors.Is(err, store.ErrOverlayNodeCredentialNotFound) {
			respondError(c, http.StatusNotFound, "node_credential_not_found", "node credential was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "credential_revoke_failed", "failed to revoke node credential")
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handler) gatewayNodeHeartbeat(c *gin.Context) {
	gatewayNoStore(c)
	credential, ok := h.requireGatewayNode(c)
	if !ok {
		return
	}
	var request gatewayHeartbeatRequest
	if !strictGatewayJSON(c, &request) {
		return
	}
	node, err := h.store.GetOverlayNode(c.Request.Context(), credential.NodeID)
	if err != nil {
		respondError(c, http.StatusForbidden, "node_scope_mismatch", "node authorization is unavailable")
		return
	}
	authorizedMode := firstNonEmpty(strings.TrimSpace(node.GatewayMode), "shadow")
	if request.NodeID != credential.NodeID || request.Mode != authorizedMode || request.ProxyCore != "xray" || strings.TrimSpace(request.AgentVersion) == "" || len(request.AgentVersion) > 128 {
		respondError(c, http.StatusForbidden, "node_scope_mismatch", "heartbeat violates the node-bound runtime mode")
		return
	}
	if request.AppliedGeneration > request.ObservedGeneration || (authorizedMode == "shadow" && request.AppliedGeneration != 0) {
		respondError(c, http.StatusConflict, "invalid_applied_generation", "applied generation is inconsistent with the authorized mode")
		return
	}
	if latest, err := h.store.GetLatestOverlayGatewaySnapshot(c.Request.Context(), credential.NodeID); err == nil && request.ObservedGeneration > latest.Generation {
		respondError(c, http.StatusConflict, "generation_unknown", "observed generation is not known to the controller")
		return
	} else if err != nil && !errors.Is(err, store.ErrOverlayGatewaySnapshotNotFound) {
		respondError(c, http.StatusServiceUnavailable, "gateway_state_unavailable", "gateway state is unavailable")
		return
	}
	if authorizedMode == "apply" && request.AppliedGeneration > 0 {
		known, err := h.store.IsOverlayGatewayGenerationApplied(c.Request.Context(), credential.NodeID, request.AppliedGeneration)
		if err != nil {
			respondError(c, http.StatusServiceUnavailable, "gateway_state_unavailable", "gateway applied state is unavailable")
			return
		}
		if !known {
			respondError(c, http.StatusConflict, "generation_unknown", "applied generation has no successful controller result")
			return
		}
	}
	heartbeat := &store.OverlayGatewayHeartbeat{NodeID: request.NodeID, AgentVersion: request.AgentVersion, Mode: request.Mode, ProxyCore: request.ProxyCore, ObservedGeneration: request.ObservedGeneration, AppliedGeneration: request.AppliedGeneration}
	if err := h.store.RecordOverlayGatewayHeartbeat(c.Request.Context(), heartbeat); err != nil {
		if errors.Is(err, store.ErrOverlayGatewayReportStale) {
			respondError(c, http.StatusConflict, "stale_gateway_report", "gateway report is stale")
			return
		}
		respondError(c, http.StatusServiceUnavailable, "gateway_heartbeat_failed", "failed to persist gateway heartbeat")
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handler) gatewayNodeSnapshot(c *gin.Context) {
	gatewayNoStore(c)
	credential, ok := h.requireGatewayNode(c)
	if !ok {
		return
	}
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID != credential.NodeID {
		respondError(c, http.StatusForbidden, "node_scope_mismatch", "credential is bound to another node")
		return
	}
	if h.overlayGatewayProjection == nil {
		respondError(c, http.StatusServiceUnavailable, "gateway_projection_unavailable", "gateway projection is not configured")
		return
	}
	snapshot, err := h.overlayGatewayProjection.Project(c.Request.Context(), nodeID)
	if errors.Is(err, gatewayprojection.ErrEmptyPeerProjection) {
		c.Status(http.StatusNoContent)
		return
	}
	if err != nil {
		respondError(c, http.StatusConflict, "gateway_projection_unsafe", "gateway snapshot could not be safely projected")
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "gateway_projection_failed", "failed to encode gateway snapshot")
		return
	}
	c.Header("ETag", `"`+snapshot.SnapshotID+`"`)
	c.Data(http.StatusOK, gatewayMediaType, raw)
}

func validGatewayDiff(diff gatewayDiffSummary) bool {
	if diff.ProjectedPeers < 0 || diff.CurrentPeers < 0 || diff.MissingPeers < 0 || diff.UnexpectedPeers < 0 || diff.RouteMismatches < 0 {
		return false
	}
	if diff.Status == "unavailable" {
		return !diff.Equal && diff.CurrentPeers == 0 && diff.MissingPeers == 0 && diff.UnexpectedPeers == 0 && diff.RouteMismatches == 0
	}
	if diff.Status != "available" || diff.MissingPeers > diff.ProjectedPeers || diff.UnexpectedPeers > diff.CurrentPeers {
		return false
	}
	commonProjected := diff.ProjectedPeers - diff.MissingPeers
	commonCurrent := diff.CurrentPeers - diff.UnexpectedPeers
	if commonProjected != commonCurrent || diff.RouteMismatches > commonProjected {
		return false
	}
	wantEqual := diff.MissingPeers == 0 && diff.UnexpectedPeers == 0 && diff.RouteMismatches == 0
	return diff.Equal == wantEqual
}
func validGatewayResult(value string) bool {
	return value == "shadow_validated" || value == "shadow_validated_wg_unavailable" || value == "shadow_rejected"
}

func validGatewayApplyModeResult(request gatewayApplyResultRequest) bool {
	if !validGatewayDiff(request.Diff) || request.ObservedGeneration == 0 {
		return false
	}
	switch request.Result {
	case "applied":
		return request.RuntimeApplied && request.AppliedGeneration == request.ObservedGeneration && request.Diff.Status == "available" && request.Diff.Equal
	case "apply_rejected", "apply_failed_rolled_back", "apply_failed_rollback_failed":
		return !request.RuntimeApplied && request.AppliedGeneration < request.ObservedGeneration
	default:
		return false
	}
}

func gatewayResultMatchesDiff(result string, diff gatewayDiffSummary) bool {
	if result == "shadow_validated" {
		return diff.Status == "available"
	}
	return (result == "shadow_validated_wg_unavailable" || result == "shadow_rejected") && diff.Status == "unavailable"
}

func (h *handler) gatewayNodeApplyResult(c *gin.Context) {
	gatewayNoStore(c)
	credential, ok := h.requireGatewayNode(c)
	if !ok {
		return
	}
	var request gatewayApplyResultRequest
	if !strictGatewayJSON(c, &request) {
		return
	}
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID != credential.NodeID || request.NodeID != nodeID {
		respondError(c, http.StatusForbidden, "node_scope_mismatch", "credential and payload must match path node")
		return
	}
	node, err := h.store.GetOverlayNode(c.Request.Context(), nodeID)
	if err != nil {
		respondError(c, http.StatusForbidden, "node_scope_mismatch", "node authorization is unavailable")
		return
	}
	authorizedMode := firstNonEmpty(strings.TrimSpace(node.GatewayMode), "shadow")
	valid := authorizedMode == "shadow" && request.ObservedGeneration > 0 && request.AppliedGeneration == 0 && !request.RuntimeApplied && validGatewayResult(request.Result) && validGatewayDiff(request.Diff) && gatewayResultMatchesDiff(request.Result, request.Diff)
	if authorizedMode == "apply" {
		valid = validGatewayApplyModeResult(request)
	}
	if !valid {
		respondError(c, http.StatusBadRequest, "invalid_apply_result", "result is inconsistent with the node-bound runtime mode")
		return
	}
	if authorizedMode == "apply" && request.AppliedGeneration > 0 && request.Result != "applied" {
		known, err := h.store.IsOverlayGatewayGenerationApplied(c.Request.Context(), nodeID, request.AppliedGeneration)
		if err != nil {
			respondError(c, http.StatusServiceUnavailable, "gateway_state_unavailable", "gateway applied state is unavailable")
			return
		}
		if !known {
			respondError(c, http.StatusConflict, "generation_unknown", "rollback checkpoint has no successful controller result")
			return
		}
	}
	diff, err := json.Marshal(request.Diff)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_apply_result", "invalid shadow evidence")
		return
	}
	result := &store.OverlayGatewayApplyResult{NodeID: nodeID, SnapshotID: request.SnapshotID, ObservedGeneration: request.ObservedGeneration, AppliedGeneration: request.AppliedGeneration, RuntimeApplied: request.RuntimeApplied, Result: request.Result, Diff: diff}
	duplicate, err := h.store.RecordOverlayGatewayApplyResult(c.Request.Context(), result)
	if err != nil {
		if errors.Is(err, store.ErrOverlayGatewayReportStale) {
			respondError(c, http.StatusConflict, "stale_gateway_report", "snapshot result is stale or conflicting")
			return
		}
		respondError(c, http.StatusServiceUnavailable, "apply_result_failed", "failed to persist apply result")
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": true, "duplicate": duplicate, "received_at": result.ReceivedAt})
}
