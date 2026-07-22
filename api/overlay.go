package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/store"
)

const (
	defaultOverlayNetworkID       = "xworkmate-private"
	defaultOverlayInterface       = "xwg0"
	defaultOverlayCIDRPrefix      = "172.29.10"
	defaultOverlayDeviceStartHost = 100
	defaultOverlayDeviceEndHost   = 254
	defaultOverlayLocalProxyPort  = 51830
	defaultOverlayMTU             = 1280
	defaultOverlayKeepalive       = 25
	overlayTransportType          = "vless-tls"
	overlayTransportSecurity      = "tls"
	defaultOverlayTransportPort   = 2443
)

type overlayDeviceRegisterRequest struct {
	DeviceID           string `json:"device_id"`
	Name               string `json:"name"`
	Platform           string `json:"platform"`
	Hostname           string `json:"hostname"`
	NetworkID          string `json:"network_id"`
	WireGuardPublicKey string `json:"wireguard_public_key"`
	WireGuardAddress   string `json:"wireguard_address"`
}

type overlayConfigAckRequest struct {
	DeviceID  string `json:"device_id"`
	NetworkID string `json:"network_id"`
	Revision  string `json:"revision"`
	Digest    string `json:"digest"`
	AppliedAt string `json:"applied_at"`
}

func (h *handler) registerOverlayRoutes(r gin.IRoutes) {
	r.GET("/networks", h.listOverlayNetworks)
	r.POST("/devices/register", h.registerOverlayDevice)
	r.GET("/devices", h.listOverlayDevices)
	r.GET("/config", h.overlayConfig)
	r.POST("/config/ack", h.overlayConfigAck)
}

func (h *handler) listOverlayNetworks(c *gin.Context) {
	if _, ok := h.requireActiveOverlayUser(c); !ok {
		return
	}
	networkID := normalizeOverlayNetworkID(c.Query("network_id"))
	c.JSON(http.StatusOK, gin.H{
		"networks": []gin.H{overlayNetworkPayload(networkID)},
	})
}

func (h *handler) registerOverlayDevice(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}

	var req overlayDeviceRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	deviceID := sanitizeOverlayID(req.DeviceID)
	if deviceID == "" {
		respondError(c, http.StatusBadRequest, "device_id_required", "device_id is required")
		return
	}
	publicKey := strings.TrimSpace(req.WireGuardPublicKey)
	if publicKey == "" {
		respondError(c, http.StatusBadRequest, "wireguard_public_key_required", "wireguard_public_key is required")
		return
	}
	if !isWireGuardKey(publicKey) {
		respondError(c, http.StatusBadRequest, "invalid_wireguard_public_key", "wireguard_public_key must be a 32-byte base64 WireGuard key")
		return
	}

	networkID := normalizeOverlayNetworkID(req.NetworkID)
	address, ok := h.assignOverlayDeviceAddress(c, user.ID, deviceID, networkID, req.WireGuardAddress)
	if !ok {
		return
	}

	now := time.Now().UTC()
	device := &store.OverlayDevice{
		ID:                 deviceID,
		UserID:             user.ID,
		NetworkID:          networkID,
		Name:               strings.TrimSpace(req.Name),
		Platform:           strings.TrimSpace(req.Platform),
		Hostname:           strings.TrimSpace(req.Hostname),
		WireGuardPublicKey: publicKey,
		WireGuardAddress:   address,
		LastSeenAt:         &now,
	}
	if device.Name == "" {
		device.Name = deviceID
	}

	if err := h.store.UpsertOverlayDevice(c.Request.Context(), device); err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_device_register_failed", "failed to register overlay device")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"device":  overlayDevicePayload(device),
		"network": overlayNetworkPayload(networkID),
	})
}

func (h *handler) assignOverlayDeviceAddress(c *gin.Context, userID, deviceID, networkID, requestedAddress string) (string, bool) {
	devices, err := h.store.ListOverlayDevicesByNetwork(c.Request.Context(), networkID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_devices_unavailable", "failed to list overlay devices")
		return "", false
	}

	requestedAddress = normalizeOverlayDeviceAddress(requestedAddress)
	if requestedAddress != "" {
		if overlayAddressInUse(devices, userID, networkID, deviceID, requestedAddress) {
			respondError(c, http.StatusConflict, "wireguard_address_in_use", "wireguard address is already assigned to another device")
			return "", false
		}
		return requestedAddress, true
	}

	for _, device := range devices {
		if device.UserID == userID && device.ID == deviceID && normalizeOverlayNetworkID(device.NetworkID) == networkID {
			address := normalizeOverlayDeviceAddress(device.WireGuardAddress)
			if address != "" {
				return address, true
			}
		}
	}

	preferred := deriveOverlayDeviceAddress(userID, deviceID)
	if !overlayAddressInUse(devices, userID, networkID, deviceID, preferred) {
		return preferred, true
	}

	prefix := envOrDefault("OVERLAY_WIREGUARD_PREFIX", defaultOverlayCIDRPrefix)
	for host := defaultOverlayDeviceStartHost; host <= defaultOverlayDeviceEndHost; host++ {
		candidate := fmt.Sprintf("%s.%d/32", prefix, host)
		if !overlayAddressInUse(devices, userID, networkID, deviceID, candidate) {
			return candidate, true
		}
	}

	respondError(c, http.StatusServiceUnavailable, "overlay_address_pool_exhausted", "no overlay wireguard addresses are available")
	return "", false
}

func (h *handler) listOverlayDevices(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}
	devices, err := h.store.ListOverlayDevicesByUser(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_devices_unavailable", "failed to list overlay devices")
		return
	}
	payload := make([]gin.H, 0, len(devices))
	for i := range devices {
		payload = append(payload, overlayDevicePayload(&devices[i]))
	}
	c.JSON(http.StatusOK, gin.H{"devices": payload})
}

func (h *handler) overlayConfig(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}

	deviceID := sanitizeOverlayID(c.Query("device_id"))
	if deviceID == "" {
		respondError(c, http.StatusBadRequest, "device_id_required", "device_id is required")
		return
	}
	networkID := normalizeOverlayNetworkID(c.Query("network_id"))

	device, err := h.store.GetOverlayDevice(c.Request.Context(), user.ID, deviceID)
	if err != nil {
		respondError(c, http.StatusNotFound, "overlay_device_not_found", "overlay device is not registered")
		return
	}
	if networkID != "" && device.NetworkID != networkID {
		respondError(c, http.StatusBadRequest, "network_mismatch", "device is registered to a different network")
		return
	}
	networkID = normalizeOverlayNetworkID(device.NetworkID)

	nodes, err := h.resolveOverlayNodes(c, networkID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_nodes_unavailable", "failed to resolve overlay nodes")
		return
	}
	if len(nodes) == 0 {
		respondError(c, http.StatusServiceUnavailable, "overlay_nodes_empty", "no overlay gateway nodes are available")
		return
	}
	requestedNodeID := sanitizeOverlayID(c.Query("node_id"))
	node, ok := selectOverlayNode(nodes, requestedNodeID)
	if !ok {
		respondError(c, http.StatusNotFound, "overlay_node_not_found", "requested overlay gateway node is not available")
		return
	}
	if strings.TrimSpace(node.TransportUUID) == "" {
		respondError(c, http.StatusServiceUnavailable, "overlay_transport_uuid_missing", "overlay gateway transport uuid is not configured")
		return
	}
	if !isUUID(node.TransportUUID) {
		respondError(c, http.StatusServiceUnavailable, "overlay_transport_uuid_invalid", "overlay gateway transport uuid is invalid")
		return
	}
	if !isValidOverlayPort(node.EndpointPort) {
		respondError(c, http.StatusServiceUnavailable, "overlay_endpoint_port_invalid", "overlay gateway endpoint port is invalid")
		return
	}
	if strings.TrimSpace(node.TransportType) != overlayTransportType {
		respondError(c, http.StatusServiceUnavailable, "overlay_transport_type_unsupported", "overlay gateway transport type is unsupported")
		return
	}
	if strings.TrimSpace(node.TransportSecurity) != overlayTransportSecurity {
		respondError(c, http.StatusServiceUnavailable, "overlay_transport_security_unsupported", "overlay gateway transport security is unsupported")
		return
	}
	localProxyPort := envIntOrDefault("OVERLAY_LOCAL_PROXY_PORT", defaultOverlayLocalProxyPort)
	if !isValidOverlayPort(localProxyPort) {
		respondError(c, http.StatusServiceUnavailable, "overlay_local_proxy_port_invalid", "overlay local proxy port is invalid")
		return
	}
	gatewayWireGuardIP := normalizeOverlayHostAddress(node.WireGuardAddress)
	revision := deriveOverlayConfigRevision(user, device, node)
	digest := digestOverlayConfig(user, device, node, revision)

	c.JSON(http.StatusOK, gin.H{
		"schema_version": 1,
		"revision":       revision,
		"digest":         digest,
		"network":        overlayNetworkPayload(networkID),
		"device":         overlayDevicePayload(device),
		"wireguard": gin.H{
			"interface":              envOrDefault("OVERLAY_WIREGUARD_INTERFACE", defaultOverlayInterface),
			"address":                device.WireGuardAddress,
			"mtu":                    envIntOrDefault("OVERLAY_WIREGUARD_MTU", defaultOverlayMTU),
			"dns":                    overlayDNSServers(),
			"private_key_ref":        "local-keychain",
			"local_proxy_endpoint":   fmt.Sprintf("127.0.0.1:%d", localProxyPort),
			"persistent_keepalive":   envIntOrDefault("OVERLAY_WIREGUARD_KEEPALIVE", defaultOverlayKeepalive),
			"peer_public_key":        node.WireGuardPublicKey,
			"peer_allowed_ips":       overlayAllowedIPs(networkID),
			"peer_endpoint":          fmt.Sprintf("127.0.0.1:%d", localProxyPort),
			"gateway_wireguard_ip":   gatewayWireGuardIP,
			"gateway_wireguard_cidr": gatewayWireGuardIP + "/32",
		},
		"transport": gin.H{
			"runtime":         envOrDefault("OVERLAY_TRANSPORT_RUNTIME", "xray-core"),
			"type":            node.TransportType,
			"security":        node.TransportSecurity,
			"server":          node.EndpointHost,
			"port":            node.EndpointPort,
			"uuid":            node.TransportUUID,
			"path":            node.TransportPath,
			"mode":            node.TransportMode,
			"flow":            strings.TrimSpace(os.Getenv("OVERLAY_TRANSPORT_FLOW")),
			"packet_encoding": envOrDefault("OVERLAY_TRANSPORT_PACKET_ENCODING", "xudp"),
			"local_port":      localProxyPort,
		},
		"nodes": []gin.H{overlayNodePayload(&node)},
	})
}

func (h *handler) overlayConfigAck(c *gin.Context) {
	user, ok := h.requireActiveOverlayUser(c)
	if !ok {
		return
	}

	var req overlayConfigAckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	if sanitizeOverlayID(req.DeviceID) == "" || strings.TrimSpace(req.Revision) == "" {
		respondError(c, http.StatusBadRequest, "invalid_ack", "device_id and revision are required")
		return
	}

	appliedAt := time.Now().UTC()
	if raw := strings.TrimSpace(req.AppliedAt); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_applied_at", "applied_at must be RFC3339")
			return
		}
		appliedAt = parsed.UTC()
	}

	ack := &store.OverlayConfigAck{
		DeviceID:  sanitizeOverlayID(req.DeviceID),
		UserID:    user.ID,
		NetworkID: normalizeOverlayNetworkID(req.NetworkID),
		Revision:  strings.TrimSpace(req.Revision),
		Digest:    strings.TrimSpace(req.Digest),
		AppliedAt: appliedAt,
	}
	if err := h.store.UpsertOverlayConfigAck(c.Request.Context(), ack); err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_ack_failed", "failed to store overlay config ack")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"acked":       true,
		"device_id":   ack.DeviceID,
		"network_id":  ack.NetworkID,
		"revision":    ack.Revision,
		"received_at": ack.ReceivedAt,
	})
}

func (h *handler) requireActiveOverlayUser(c *gin.Context) (*store.User, bool) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return nil, false
	}
	if !user.Active {
		respondError(c, http.StatusForbidden, "account_paused", "account is paused")
		return nil, false
	}
	return user, true
}

func (h *handler) resolveOverlayNodes(c *gin.Context, networkID string) ([]store.OverlayNode, error) {
	nodes, err := h.store.ListOverlayNodes(c.Request.Context(), networkID)
	if err != nil {
		return nil, err
	}
	if len(nodes) > 0 {
		return nodes, nil
	}
	return []store.OverlayNode{defaultOverlayNode(networkID)}, nil
}

func selectOverlayNode(nodes []store.OverlayNode, requestedNodeID string) (store.OverlayNode, bool) {
	requestedNodeID = strings.TrimSpace(requestedNodeID)
	if requestedNodeID != "" {
		for _, node := range nodes {
			if strings.TrimSpace(node.ID) == requestedNodeID {
				return node, true
			}
		}
		return store.OverlayNode{}, false
	}

	preferredID := envOrDefault("OVERLAY_GATEWAY_ID", "xworkmate-bridge")
	for _, node := range nodes {
		if strings.TrimSpace(node.ID) == preferredID && node.Healthy {
			return node, true
		}
	}
	for _, node := range nodes {
		if strings.TrimSpace(node.ID) == preferredID {
			return node, true
		}
	}
	for _, node := range nodes {
		if node.Healthy {
			return node, true
		}
	}
	if len(nodes) == 0 {
		return store.OverlayNode{}, false
	}
	return nodes[0], true
}

func defaultOverlayNode(networkID string) store.OverlayNode {
	return store.OverlayNode{
		ID:                 envOrDefault("OVERLAY_GATEWAY_ID", "xworkmate-bridge"),
		NetworkID:          normalizeOverlayNetworkID(networkID),
		Name:               envOrDefault("OVERLAY_GATEWAY_NAME", "XWorkmate Bridge"),
		Role:               envOrDefault("OVERLAY_GATEWAY_ROLE", "gateway"),
		Region:             envOrDefault("OVERLAY_GATEWAY_REGION", "jp"),
		WireGuardPublicKey: envOrDefault("OVERLAY_GATEWAY_WG_PUBLIC_KEY", "1staGq8lmHFRFRFNj2QOFx/MPxb/1fFV4tawC6xSi1Q="),
		WireGuardAddress:   normalizeOverlayHostAddress(envOrDefault("OVERLAY_GATEWAY_WG_ADDRESS", defaultOverlayCIDRPrefix+".1")),
		EndpointHost:       envOrDefault("OVERLAY_GATEWAY_HOST", "xworkmate-bridge.svc.plus"),
		EndpointPort:       envIntOrDefault("OVERLAY_GATEWAY_PORT", defaultOverlayTransportPort),
		TransportType:      envOrDefault("OVERLAY_TRANSPORT_TYPE", overlayTransportType),
		TransportSecurity:  envOrDefault("OVERLAY_TRANSPORT_SECURITY", overlayTransportSecurity),
		TransportPath:      os.Getenv("OVERLAY_TRANSPORT_PATH"),
		TransportMode:      os.Getenv("OVERLAY_TRANSPORT_MODE"),
		TransportUUID:      strings.TrimSpace(os.Getenv("OVERLAY_TRANSPORT_UUID")),
		Healthy:            true,
	}
}

func overlayDevicePayload(device *store.OverlayDevice) gin.H {
	return gin.H{
		"id":                   device.ID,
		"user_id":              device.UserID,
		"network_id":           device.NetworkID,
		"name":                 device.Name,
		"platform":             device.Platform,
		"hostname":             device.Hostname,
		"wireguard_public_key": device.WireGuardPublicKey,
		"wireguard_address":    device.WireGuardAddress,
		"created_at":           device.CreatedAt,
		"updated_at":           device.UpdatedAt,
		"last_seen_at":         device.LastSeenAt,
	}
}

func overlayNodePayload(node *store.OverlayNode) gin.H {
	return gin.H{
		"id":                   node.ID,
		"network_id":           node.NetworkID,
		"name":                 node.Name,
		"role":                 node.Role,
		"region":               node.Region,
		"wireguard_public_key": node.WireGuardPublicKey,
		"wireguard_address":    node.WireGuardAddress,
		"endpoint_host":        node.EndpointHost,
		"endpoint_port":        node.EndpointPort,
		"transport_type":       node.TransportType,
		"transport_security":   node.TransportSecurity,
		"transport_path":       node.TransportPath,
		"transport_mode":       node.TransportMode,
		"transport_uuid_set":   strings.TrimSpace(node.TransportUUID) != "",
		"healthy":              node.Healthy,
	}
}

func overlayNetworkPayload(networkID string) gin.H {
	return gin.H{
		"id":           normalizeOverlayNetworkID(networkID),
		"display_name": envOrDefault("OVERLAY_NETWORK_NAME", "XWorkmate Private"),
		"cidr":         envOrDefault("OVERLAY_NETWORK_CIDR", defaultOverlayCIDRPrefix+".0/24"),
	}
}

func sanitizeOverlayID(value string) string {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.ReplaceAll(trimmed, " ", "-")
	return strings.ToLower(trimmed)
}

func normalizeOverlayNetworkID(value string) string {
	trimmed := sanitizeOverlayID(value)
	if trimmed == "" {
		return defaultOverlayNetworkID
	}
	return trimmed
}

func deriveOverlayDeviceAddress(userID, deviceID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(userID) + ":" + strings.TrimSpace(deviceID)))
	host := defaultOverlayDeviceStartHost + int(sum[0])%100
	return fmt.Sprintf("%s.%d/32", envOrDefault("OVERLAY_WIREGUARD_PREFIX", defaultOverlayCIDRPrefix), host)
}

func normalizeOverlayDeviceAddress(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	if strings.Contains(address, "/") {
		return address
	}
	return address + "/32"
}

func normalizeOverlayHostAddress(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	if before, _, ok := strings.Cut(address, "/"); ok {
		return strings.TrimSpace(before)
	}
	return address
}

func isWireGuardKey(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	return err == nil && len(decoded) == 32
}

func isUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

func isValidOverlayPort(port int) bool {
	return port >= 1 && port <= 65535
}

func overlayAddressInUse(devices []store.OverlayDevice, userID, networkID, deviceID, address string) bool {
	networkID = normalizeOverlayNetworkID(networkID)
	address = normalizeOverlayDeviceAddress(address)
	for _, device := range devices {
		if normalizeOverlayNetworkID(device.NetworkID) != networkID {
			continue
		}
		if device.UserID == userID && device.ID == deviceID {
			continue
		}
		if normalizeOverlayDeviceAddress(device.WireGuardAddress) == address {
			return true
		}
	}
	return false
}

func overlayAllowedIPs(networkID string) []string {
	if raw := strings.TrimSpace(os.Getenv("OVERLAY_ALLOWED_IPS")); raw != "" {
		return splitCSV(raw)
	}
	_ = networkID
	return []string{envOrDefault("OVERLAY_NETWORK_CIDR", defaultOverlayCIDRPrefix+".0/24")}
}

func overlayDNSServers() []string {
	if raw := strings.TrimSpace(os.Getenv("OVERLAY_DNS")); raw != "" {
		return splitCSV(raw)
	}
	return []string{envOrDefault("OVERLAY_GATEWAY_WG_ADDRESS", defaultOverlayCIDRPrefix+".1")}
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func deriveOverlayConfigRevision(user *store.User, device *store.OverlayDevice, node store.OverlayNode) string {
	updatedAt := user.UpdatedAt
	if device.UpdatedAt.After(updatedAt) {
		updatedAt = device.UpdatedAt
	}
	if node.UpdatedAt.After(updatedAt) {
		updatedAt = node.UpdatedAt
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	return strconv.FormatInt(updatedAt.UTC().Unix(), 10)
}

func digestOverlayConfig(user *store.User, device *store.OverlayDevice, node store.OverlayNode, revision string) string {
	payload, _ := json.Marshal(gin.H{
		"user_id":   user.ID,
		"device_id": device.ID,
		"network":   device.NetworkID,
		"node":      node.ID,
		"revision":  revision,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
