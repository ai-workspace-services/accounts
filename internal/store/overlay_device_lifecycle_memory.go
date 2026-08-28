package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

func (s *memoryStore) RotateOverlayDeviceKey(_ context.Context, userID, networkID, deviceID, newKey string, expected uint64, audit *AuditLog) (*OverlayDevice, bool, error) {
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	device := s.overlayDevices[overlayDeviceKey(userID, deviceID)]
	if device == nil || device.NetworkID != networkID {
		return nil, false, ErrOverlayDeviceNotFound
	}
	if device.Status == OverlayDeviceRevoked {
		return nil, false, ErrOverlayDeviceRevoked
	}
	if device.WireGuardPublicKey == newKey {
		return cloneOverlayDevice(device), true, nil
	}
	if expected == 0 || device.KeyVersion != expected {
		return nil, false, ErrOverlayDeviceVersionConflict
	}
	for _, other := range s.overlayDevices {
		if other.NetworkID == networkID && other.ID != deviceID && other.Status != OverlayDeviceRevoked && other.WireGuardPublicKey == newKey {
			return nil, false, ErrOverlayDeviceKeyConflict
		}
	}
	now := time.Now().UTC()
	if err := s.claimOverlayDeviceKeyLocked(networkID, strings.TrimSpace(newKey), userID, deviceID, device.KeyVersion+1, now); err != nil {
		return nil, false, err
	}
	device.WireGuardPublicKey = strings.TrimSpace(newKey)
	device.KeyVersion++
	device.StateVersion++
	device.UpdatedAt = now
	s.appendOverlayDeviceEventLocked(device, "key_rotated", now)
	audit.CreatedAt = now
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return cloneOverlayDevice(device), false, nil
}
func (s *memoryStore) SetOverlayDeviceStatus(_ context.Context, userID, networkID, deviceID, status string, expected uint64, reason string, audit *AuditLog) (*OverlayDevice, bool, error) {
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	if status != OverlayDeviceActive && status != OverlayDeviceInactive && status != OverlayDeviceRevoked {
		return nil, false, errors.New("invalid overlay device status")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	device := s.overlayDevices[overlayDeviceKey(userID, deviceID)]
	if device == nil || device.NetworkID != networkID {
		return nil, false, ErrOverlayDeviceNotFound
	}
	if device.Status == OverlayDeviceRevoked {
		if status == OverlayDeviceRevoked {
			return cloneOverlayDevice(device), true, nil
		}
		return nil, false, ErrOverlayDeviceRevoked
	}
	if device.Status == status {
		return cloneOverlayDevice(device), true, nil
	}
	if expected == 0 || device.StateVersion != expected {
		return nil, false, ErrOverlayDeviceVersionConflict
	}
	device.Status = status
	device.StateVersion++
	now := time.Now().UTC()
	device.UpdatedAt = now
	eventType := "status_changed"
	if status == OverlayDeviceRevoked {
		eventType = "revoked"
		device.RevokedAt = &now
		device.RevokedReason = strings.TrimSpace(reason)
		for _, token := range s.overlayJoinTokens {
			if token.UserID == userID && token.NetworkID == networkID && token.DeviceID == deviceID && token.RevokedAt == nil {
				v := now
				token.RevokedAt = &v
			}
		}
		for hash, session := range s.overlayEnrollments {
			if session.UserID == userID && session.NetworkID == networkID && session.DeviceID == deviceID {
				delete(s.overlayEnrollments, hash)
			}
		}
	}
	s.appendOverlayDeviceEventLocked(device, eventType, now)
	audit.CreatedAt = now
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return cloneOverlayDevice(device), false, nil
}
func (s *memoryStore) ListOverlayDeviceEvents(_ context.Context, userID, networkID string, after uint64, limit int) ([]OverlayDeviceEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []OverlayDeviceEvent{}
	for _, event := range s.overlayDeviceEvents {
		if event.Sequence <= after || event.UserID != userID || (networkID != "" && event.NetworkID != networkID) {
			continue
		}
		out = append(out, event)
		if len(out) == limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}
