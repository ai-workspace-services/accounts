package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/overlay/domain"
	"account/internal/overlay/projection"
)

const maxSignedConfigAckBody = 64 << 10

type overlaySignedConfigAckRequest struct {
	ConfigID  string `json:"config_id"`
	DeviceID  string `json:"device_id"`
	AppliedAt string `json:"applied_at,omitempty"`
}

func newOverlayProjectionServiceFromEnvironment() (*projection.Service, error) {
	encodedPrivateKey := strings.TrimSpace(os.Getenv("OVERLAY_SIGNING_PRIVATE_KEY"))
	if encodedPrivateKey == "" {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("OVERLAY_PROJECTION_ALLOW_MEMORY")), "true") {
		return nil, errors.New("durable overlay projection repository is not configured; memory repository is allowed only with OVERLAY_PROJECTION_ALLOW_MEMORY=true")
	}
	keyID := envOrDefault("OVERLAY_SIGNING_KEY_ID", "overlay_signing_key_01")
	signer, err := projection.NewEd25519SignerFromBase64(encodedPrivateKey, keyID)
	if err != nil {
		return nil, fmt.Errorf("configure overlay signing key: %w", err)
	}
	lifetime := 24 * time.Hour
	if raw := strings.TrimSpace(os.Getenv("OVERLAY_SIGNED_CONFIG_TTL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return nil, errors.New("OVERLAY_SIGNED_CONFIG_TTL must be a positive duration")
		}
		lifetime = parsed
	}
	clock := time.Now
	repository := projection.NewMemoryRepository(clock)
	return projection.NewService(repository, signer, clock, lifetime)
}

func (h *handler) overlaySignedConfig(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}
	if h.overlayProjection == nil {
		respondError(c, http.StatusServiceUnavailable, "overlay_projection_unavailable", "signed config projection is not configured")
		return
	}
	deviceID := sanitizeOverlayID(c.Query("device_id"))
	if deviceID == "" {
		respondError(c, http.StatusBadRequest, "device_id_required", "device_id is required")
		return
	}
	device, err := h.store.GetOverlayDevice(c.Request.Context(), user.ID, deviceID)
	if err != nil {
		respondError(c, http.StatusNotFound, "overlay_device_not_found", "overlay device is not registered")
		return
	}
	networkID := normalizeOverlayNetworkID(device.NetworkID)
	if requested := strings.TrimSpace(c.Query("network_id")); requested != "" && normalizeOverlayNetworkID(requested) != networkID {
		respondError(c, http.StatusBadRequest, "network_mismatch", "device is registered to a different network")
		return
	}
	nodes, err := h.resolveOverlayNodes(c, networkID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_nodes_unavailable", "failed to resolve overlay nodes")
		return
	}
	node, found := selectOverlayNode(nodes, sanitizeOverlayID(c.Query("node_id")))
	if !found {
		respondError(c, http.StatusServiceUnavailable, "overlay_node_not_found", "no overlay gateway node is available")
		return
	}
	if node.TransportType != overlayTransportType || node.TransportSecurity != overlayTransportSecurity || !isUUID(node.TransportUUID) {
		respondError(c, http.StatusServiceUnavailable, "overlay_transport_invalid", "gateway transport is not compatible with XConnect-One v1")
		return
	}
	input := projection.Input{
		UserID:                     user.ID,
		NetworkID:                  networkID,
		DeviceID:                   device.ID,
		DeviceAddresses:            []string{device.WireGuardAddress},
		GatewayID:                  node.ID,
		GatewayPublicKey:           node.WireGuardPublicKey,
		AllowedIPs:                 overlayAllowedIPs(networkID),
		InterfaceName:              envOrDefault("OVERLAY_WIREGUARD_INTERFACE", defaultOverlayInterface),
		MTU:                        envIntOrDefault("OVERLAY_WIREGUARD_MTU", defaultOverlayMTU),
		LoopbackPort:               envIntOrDefault("OVERLAY_LOCAL_PROXY_PORT", defaultOverlayLocalProxyPort),
		PersistentKeepaliveSeconds: envIntOrDefault("OVERLAY_WIREGUARD_KEEPALIVE", defaultOverlayKeepalive),
		RemoteHost:                 node.EndpointHost,
		RemotePort:                 node.EndpointPort,
		ServerName:                 envOrDefault("OVERLAY_TRANSPORT_SERVER_NAME", node.EndpointHost),
		AuthID:                     node.TransportUUID,
	}
	input.SourceRevision = signedConfigSourceRevision(input)
	config, err := h.overlayProjection.Project(c.Request.Context(), input)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "overlay_projection_failed", "failed to project signed overlay config")
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", `"`+config.ConfigID+`"`)
	c.JSON(http.StatusOK, config)
}

func (h *handler) overlaySignedConfigAck(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}
	if h.overlayProjection == nil {
		respondError(c, http.StatusServiceUnavailable, "overlay_projection_unavailable", "signed config projection is not configured")
		return
	}
	generation, err := strconv.ParseUint(c.Param("generation"), 10, 64)
	if err != nil || generation == 0 {
		respondError(c, http.StatusBadRequest, "invalid_generation", "generation must be a positive integer")
		return
	}
	request, err := decodeOverlaySignedConfigAck(c)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_ack", err.Error())
		return
	}
	appliedAt := time.Time{}
	if strings.TrimSpace(request.AppliedAt) != "" {
		appliedAt, err = time.Parse(time.RFC3339, request.AppliedAt)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_applied_at", "applied_at must be RFC3339")
			return
		}
	}
	result, err := h.overlayProjection.Acknowledge(c.Request.Context(), projection.Ack{
		UserID: user.ID, DeviceID: sanitizeOverlayID(request.DeviceID), ConfigID: strings.TrimSpace(request.ConfigID),
		Generation: generation, AppliedAt: appliedAt.UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, projection.ErrStaleGeneration):
			respondError(c, http.StatusConflict, "stale_generation", "configuration generation is no longer current")
		case errors.Is(err, projection.ErrConfigMismatch):
			respondError(c, http.StatusConflict, "config_mismatch", "config_id does not match generation")
		case errors.Is(err, projection.ErrConfigNotFound):
			respondError(c, http.StatusNotFound, "signed_config_not_found", "signed configuration generation was not found")
		case errors.Is(err, projection.ErrAppliedAtFuture):
			respondError(c, http.StatusBadRequest, "applied_at_in_future", "applied_at is too far in the future")
		default:
			respondError(c, http.StatusInternalServerError, "overlay_ack_failed", "failed to persist signed config acknowledgement")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": true, "duplicate": result.Duplicate, "ack": result.Ack})
}

func decodeOverlaySignedConfigAck(c *gin.Context) (overlaySignedConfigAckRequest, error) {
	var request overlaySignedConfigAckRequest
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxSignedConfigAckBody))
	if err != nil {
		return request, errors.New("ack payload is too large")
	}
	if err := domain.ValidateNoSecretFields(raw); err != nil {
		return request, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("invalid acknowledgement payload")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return request, errors.New("ack payload must contain one JSON object")
	}
	if strings.TrimSpace(request.ConfigID) == "" || sanitizeOverlayID(request.DeviceID) == "" {
		return request, errors.New("config_id and device_id are required")
	}
	return request, nil
}

func signedConfigSourceRevision(input projection.Input) string {
	input.SourceRevision = ""
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
