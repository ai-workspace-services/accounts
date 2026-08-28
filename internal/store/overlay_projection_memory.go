package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

func (s *memoryStore) OverlayProjectionDurable() bool { return false }

func (s *memoryStore) GetOverlaySigningKeyMaxExpiresAt(_ context.Context, keyID string) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest time.Time
	for _, record := range s.overlaySignedConfigs {
		if record.SigningKeyID == keyID && record.ExpiresAt.After(latest) {
			latest = record.ExpiresAt
		}
	}
	for _, records := range s.overlayGatewaySnapshots {
		for _, record := range records {
			if record.SigningKeyID == keyID && record.ExpiresAt.After(latest) {
				latest = record.ExpiresAt
			}
		}
	}
	return latest.UTC(), nil
}

func cloneOverlaySignedConfigRecord(src *OverlaySignedConfigRecord) *OverlaySignedConfigRecord {
	if src == nil {
		return nil
	}
	clone := *src
	clone.SignedPayload = append([]byte(nil), src.SignedPayload...)
	if src.Ack != nil {
		ack := *src.Ack
		clone.Ack = &ack
	}
	return &clone
}

func (s *memoryStore) GetLatestOverlaySignedConfig(_ context.Context, userID, deviceID string) (*OverlaySignedConfigRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record := s.overlaySignedConfigs[overlayDeviceKey(userID, deviceID)]
	if record == nil {
		return nil, ErrOverlaySignedConfigNotFound
	}
	return cloneOverlaySignedConfigRecord(record), nil
}

func (s *memoryStore) SaveOverlaySignedConfig(_ context.Context, record *OverlaySignedConfigRecord) error {
	if record == nil || strings.TrimSpace(record.UserID) == "" || strings.TrimSpace(record.DeviceID) == "" || record.Generation == 0 {
		return errors.New("valid overlay signed config record is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := overlayDeviceKey(record.UserID, record.DeviceID)
	current := s.overlaySignedConfigs[key]
	if current == nil {
		if record.Generation != 1 {
			return ErrOverlaySignedConfigGap
		}
	} else {
		if current.SourceRevision == record.SourceRevision {
			return ErrOverlaySignedConfigStale
		}
		if record.Generation <= current.Generation {
			return ErrOverlaySignedConfigStale
		}
		if record.Generation != current.Generation+1 {
			return ErrOverlaySignedConfigGap
		}
	}
	stored := cloneOverlaySignedConfigRecord(record)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	s.overlaySignedConfigs[key] = stored
	*record = *cloneOverlaySignedConfigRecord(stored)
	return nil
}

func (s *memoryStore) AcknowledgeOverlaySignedConfig(_ context.Context, ack *OverlaySignedConfigAck) (bool, error) {
	if ack == nil {
		return false, errors.New("overlay signed config acknowledgement is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := overlayDeviceKey(ack.UserID, ack.DeviceID)
	record := s.overlaySignedConfigs[key]
	if record == nil || ack.Generation > record.Generation {
		return false, ErrOverlaySignedConfigNotFound
	}
	if ack.Generation < record.Generation {
		return false, ErrOverlaySignedConfigStale
	}
	if ack.ConfigID != record.ConfigID {
		return false, ErrOverlaySignedConfigMismatch
	}
	if record.Ack != nil {
		*ack = *record.Ack
		return true, nil
	}
	record.Ack = ack
	*ack = *record.Ack
	return false, nil
}
