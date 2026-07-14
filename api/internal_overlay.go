package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

type internalOverlayNodeHeartbeatRequest struct {
	NodeID             string `json:"node_id"`
	NetworkID          string `json:"network_id"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	Region             string `json:"region"`
	WireGuardPublicKey string `json:"wireguard_public_key"`
	WireGuardAddress   string `json:"wireguard_address"`
	EndpointHost       string `json:"endpoint_host"`
	EndpointPort       int    `json:"endpoint_port"`
	TransportType      string `json:"transport_type"`
	TransportSecurity  string `json:"transport_security"`
	TransportPath      string `json:"transport_path"`
	TransportMode      string `json:"transport_mode"`
	TransportUUID      string `json:"transport_uuid"`
	Healthy            *bool  `json:"healthy"`
	SampledAt          string `json:"sampled_at"`
}

func (h *handler) internalOverlayNodeHeartbeat(c *gin.Context) {
	if h.store == nil {
		respondError(c, http.StatusServiceUnavailable, "store_unavailable", "overlay store is not available")
		return
	}

	var req internalOverlayNodeHeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid overlay node heartbeat payload")
		return
	}

	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "node_id_required", "node_id is required")
		return
	}
	if strings.TrimSpace(req.WireGuardPublicKey) == "" {
		respondError(c, http.StatusBadRequest, "wireguard_public_key_required", "wireguard_public_key is required")
		return
	}
	if !isWireGuardKey(req.WireGuardPublicKey) {
		respondError(c, http.StatusBadRequest, "invalid_wireguard_public_key", "wireguard_public_key must be a 32-byte base64 WireGuard key")
		return
	}
	if strings.TrimSpace(req.WireGuardAddress) == "" {
		respondError(c, http.StatusBadRequest, "wireguard_address_required", "wireguard_address is required")
		return
	}
	if strings.TrimSpace(req.EndpointHost) == "" {
		respondError(c, http.StatusBadRequest, "endpoint_host_required", "endpoint_host is required")
		return
	}
	if strings.TrimSpace(req.TransportUUID) == "" {
		respondError(c, http.StatusBadRequest, "transport_uuid_required", "transport_uuid is required")
		return
	}
	if !isUUID(req.TransportUUID) {
		respondError(c, http.StatusBadRequest, "invalid_transport_uuid", "transport_uuid must be a valid UUID")
		return
	}

	sampledAt, err := parseOptionalTime(req.SampledAt)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_sampled_at", "sampled_at must be RFC3339")
		return
	}
	if sampledAt.IsZero() {
		sampledAt = time.Now().UTC()
	}
	healthy := true
	if req.Healthy != nil {
		healthy = *req.Healthy
	}
	endpointPort := req.EndpointPort
	if endpointPort == 0 {
		endpointPort = defaultOverlayTransportPort
	}
	if !isValidOverlayPort(endpointPort) {
		respondError(c, http.StatusBadRequest, "invalid_endpoint_port", "endpoint_port must be between 1 and 65535")
		return
	}
	transportType := firstNonEmpty(strings.TrimSpace(req.TransportType), overlayTransportType)
	if transportType != overlayTransportType {
		respondError(c, http.StatusBadRequest, "unsupported_transport_type", "transport_type must be vless-tls")
		return
	}
	transportSecurity := firstNonEmpty(strings.TrimSpace(req.TransportSecurity), overlayTransportSecurity)
	if transportSecurity != overlayTransportSecurity {
		respondError(c, http.StatusBadRequest, "unsupported_transport_security", "transport_security must be tls")
		return
	}

	node := &store.OverlayNode{
		ID:                 nodeID,
		NetworkID:          normalizeOverlayNetworkID(req.NetworkID),
		Name:               strings.TrimSpace(req.Name),
		Role:               firstNonEmpty(strings.TrimSpace(req.Role), "gateway"),
		Region:             strings.TrimSpace(req.Region),
		WireGuardPublicKey: strings.TrimSpace(req.WireGuardPublicKey),
		WireGuardAddress:   normalizeOverlayHostAddress(req.WireGuardAddress),
		EndpointHost:       strings.TrimSpace(req.EndpointHost),
		EndpointPort:       endpointPort,
		TransportType:      transportType,
		TransportSecurity:  transportSecurity,
		TransportPath:      strings.TrimSpace(req.TransportPath),
		TransportMode:      strings.TrimSpace(req.TransportMode),
		TransportUUID:      strings.TrimSpace(req.TransportUUID),
		Healthy:            healthy,
		LastHeartbeat:      &sampledAt,
	}
	if node.Name == "" {
		node.Name = node.ID
	}

	if err := h.store.UpsertOverlayNode(c.Request.Context(), node); err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_node_heartbeat_failed", "failed to persist overlay node heartbeat")
		return
	}

	c.JSON(http.StatusOK, gin.H{"node": overlayNodePayload(node)})
}
