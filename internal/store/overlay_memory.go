package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

var ErrOverlayDeviceNotFound = errors.New("overlay device not found")

func overlayDeviceKey(userID, deviceID string) string {
	return strings.TrimSpace(userID) + "\x00" + strings.TrimSpace(deviceID)
}

func cloneOverlayDevice(src *OverlayDevice) *OverlayDevice {
	if src == nil {
		return nil
	}
	clone := *src
	if src.LastSeenAt != nil {
		lastSeen := src.LastSeenAt.UTC()
		clone.LastSeenAt = &lastSeen
	}
	if src.RevokedAt != nil {
		v := src.RevokedAt.UTC()
		clone.RevokedAt = &v
	}
	return &clone
}

func cloneOverlayNode(src *OverlayNode) *OverlayNode {
	if src == nil {
		return nil
	}
	clone := *src
	if src.LastHeartbeat != nil {
		lastHeartbeat := src.LastHeartbeat.UTC()
		clone.LastHeartbeat = &lastHeartbeat
	}
	return &clone
}

func cloneOverlayConfigAck(src *OverlayConfigAck) *OverlayConfigAck {
	if src == nil {
		return nil
	}
	clone := *src
	return &clone
}

func (s *memoryStore) UpsertOverlayDevice(ctx context.Context, device *OverlayDevice) error {
	_ = ctx
	if device == nil {
		return errors.New("overlay device is required")
	}
	userID := strings.TrimSpace(device.UserID)
	deviceID := strings.TrimSpace(device.ID)
	if userID == "" || deviceID == "" {
		return errors.New("overlay device user_id and id are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	key := overlayDeviceKey(userID, deviceID)
	stored, exists := s.overlayDevices[key]
	if !exists {
		if err := s.claimOverlayDeviceKeyLocked(strings.TrimSpace(device.NetworkID), strings.TrimSpace(device.WireGuardPublicKey), userID, deviceID, 1, now); err != nil {
			return err
		}
		stored = &OverlayDevice{
			ID:        deviceID,
			UserID:    userID,
			CreatedAt: now,
			Status:    OverlayDeviceActive, StateVersion: 1, KeyVersion: 1,
		}
		s.overlayDevices[key] = stored
	} else {
		if stored.Status == OverlayDeviceRevoked {
			return ErrOverlayDeviceRevoked
		}
		if stored.NetworkID != "" && strings.TrimSpace(device.NetworkID) != stored.NetworkID {
			return ErrOverlayJoinConstraint
		}
		if stored.WireGuardPublicKey != "" && strings.TrimSpace(device.WireGuardPublicKey) != stored.WireGuardPublicKey {
			return ErrOverlayDeviceKeyConflict
		}
	}

	stored.NetworkID = strings.TrimSpace(device.NetworkID)
	stored.Name = strings.TrimSpace(device.Name)
	stored.Platform = strings.TrimSpace(device.Platform)
	stored.Hostname = strings.TrimSpace(device.Hostname)
	stored.WireGuardPublicKey = strings.TrimSpace(device.WireGuardPublicKey)
	stored.WireGuardAddress = strings.TrimSpace(device.WireGuardAddress)
	if device.LastSeenAt != nil {
		lastSeen := device.LastSeenAt.UTC()
		stored.LastSeenAt = &lastSeen
	}
	stored.UpdatedAt = now
	if !exists {
		s.appendOverlayDeviceEventLocked(stored, "registered", now)
	}

	*device = *cloneOverlayDevice(stored)
	return nil
}

func overlayDeviceHistoryKey(networkID, publicKey string) string {
	return strings.TrimSpace(networkID) + "\x00" + strings.TrimSpace(publicKey)
}

func (s *memoryStore) claimOverlayDeviceKeyLocked(networkID, publicKey, userID, deviceID string, keyVersion uint64, now time.Time) error {
	historyKey := overlayDeviceHistoryKey(networkID, publicKey)
	if _, exists := s.overlayDeviceKeyHistory[historyKey]; exists {
		return ErrOverlayDeviceKeyConflict
	}
	s.overlayDeviceKeyHistory[historyKey] = OverlayDeviceKeyHistory{NetworkID: strings.TrimSpace(networkID), PublicKey: strings.TrimSpace(publicKey), UserID: strings.TrimSpace(userID), DeviceID: strings.TrimSpace(deviceID), KeyVersion: keyVersion, ClaimedAt: now.UTC()}
	return nil
}

func (s *memoryStore) appendOverlayDeviceEventLocked(device *OverlayDevice, eventType string, now time.Time) {
	s.overlayDeviceEventSequence++
	s.overlayDeviceEvents = append(s.overlayDeviceEvents, OverlayDeviceEvent{Sequence: s.overlayDeviceEventSequence, UserID: device.UserID, NetworkID: device.NetworkID, DeviceID: device.ID, Type: eventType, Status: device.Status, StateVersion: device.StateVersion, KeyVersion: device.KeyVersion, CreatedAt: now.UTC()})
}

func (s *memoryStore) GetOverlayDevice(ctx context.Context, userID, deviceID string) (*OverlayDevice, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	device, ok := s.overlayDevices[overlayDeviceKey(userID, deviceID)]
	if !ok {
		return nil, ErrOverlayDeviceNotFound
	}
	return cloneOverlayDevice(device), nil
}

func (s *memoryStore) ListOverlayDevicesByUser(ctx context.Context, userID string) ([]OverlayDevice, error) {
	_ = ctx
	normalizedUserID := strings.TrimSpace(userID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]OverlayDevice, 0)
	for _, device := range s.overlayDevices {
		if device.UserID != normalizedUserID {
			continue
		}
		result = append(result, *cloneOverlayDevice(device))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *memoryStore) ListOverlayDevicesByNetwork(ctx context.Context, networkID string) ([]OverlayDevice, error) {
	_ = ctx
	normalizedNetworkID := strings.TrimSpace(networkID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]OverlayDevice, 0)
	for _, device := range s.overlayDevices {
		if normalizedNetworkID != "" && device.NetworkID != normalizedNetworkID {
			continue
		}
		result = append(result, *cloneOverlayDevice(device))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UserID == result[j].UserID {
			return result[i].ID < result[j].ID
		}
		return result[i].UserID < result[j].UserID
	})
	return result, nil
}

func (s *memoryStore) UpsertOverlayNode(ctx context.Context, node *OverlayNode) error {
	_ = ctx
	if node == nil {
		return errors.New("overlay node is required")
	}
	nodeID := strings.TrimSpace(node.ID)
	if nodeID == "" {
		return errors.New("overlay node id is required")
	}
	mode := strings.TrimSpace(node.GatewayMode)
	if mode != "" && mode != "shadow" && mode != "apply" {
		return errors.New("overlay node gateway mode must be shadow or apply")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	stored, exists := s.overlayNodes[nodeID]
	if !exists {
		stored = &OverlayNode{
			ID:        nodeID,
			CreatedAt: now,
		}
		s.overlayNodes[nodeID] = stored
	}

	stored.NetworkID = strings.TrimSpace(node.NetworkID)
	stored.Name = strings.TrimSpace(node.Name)
	stored.Role = strings.TrimSpace(node.Role)
	stored.Region = strings.TrimSpace(node.Region)
	stored.WireGuardPublicKey = strings.TrimSpace(node.WireGuardPublicKey)
	stored.WireGuardAddress = strings.TrimSpace(node.WireGuardAddress)
	stored.EndpointHost = strings.TrimSpace(node.EndpointHost)
	stored.EndpointPort = node.EndpointPort
	stored.TransportType = strings.TrimSpace(node.TransportType)
	stored.TransportSecurity = strings.TrimSpace(node.TransportSecurity)
	stored.TransportPath = strings.TrimSpace(node.TransportPath)
	stored.TransportMode = strings.TrimSpace(node.TransportMode)
	stored.TransportUUID = strings.TrimSpace(node.TransportUUID)
	stored.GatewayMode = mode
	if stored.GatewayMode == "" {
		stored.GatewayMode = "shadow"
	}
	stored.Healthy = node.Healthy
	if node.LastHeartbeat != nil {
		lastHeartbeat := node.LastHeartbeat.UTC()
		stored.LastHeartbeat = &lastHeartbeat
	}
	stored.UpdatedAt = now

	*node = *cloneOverlayNode(stored)
	return nil
}

func (s *memoryStore) ListOverlayNodes(ctx context.Context, networkID string) ([]OverlayNode, error) {
	_ = ctx
	normalizedNetworkID := strings.TrimSpace(networkID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]OverlayNode, 0)
	for _, node := range s.overlayNodes {
		if normalizedNetworkID != "" && node.NetworkID != normalizedNetworkID {
			continue
		}
		result = append(result, *cloneOverlayNode(node))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *memoryStore) UpsertOverlayConfigAck(ctx context.Context, ack *OverlayConfigAck) error {
	_ = ctx
	if ack == nil {
		return errors.New("overlay config ack is required")
	}
	if strings.TrimSpace(ack.UserID) == "" || strings.TrimSpace(ack.DeviceID) == "" {
		return errors.New("overlay config ack user_id and device_id are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored := cloneOverlayConfigAck(ack)
	stored.ReceivedAt = time.Now().UTC()
	s.overlayConfigAcks[overlayDeviceKey(stored.UserID, stored.DeviceID)] = stored
	*ack = *cloneOverlayConfigAck(stored)
	return nil
}
