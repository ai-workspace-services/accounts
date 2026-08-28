package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
)

func cloneOverlayNodeCredential(src *OverlayNodeCredential) *OverlayNodeCredential {
	if src == nil {
		return nil
	}
	clone := *src
	clone.TokenHash = append([]byte(nil), src.TokenHash...)
	if src.RevokedAt != nil {
		value := src.RevokedAt.UTC()
		clone.RevokedAt = &value
	}
	if src.LastUsedAt != nil {
		value := src.LastUsedAt.UTC()
		clone.LastUsedAt = &value
	}
	return &clone
}

func cloneOverlayGatewaySnapshot(src *OverlayGatewaySnapshotRecord) *OverlayGatewaySnapshotRecord {
	if src == nil {
		return nil
	}
	clone := *src
	clone.SignedPayload = append([]byte(nil), src.SignedPayload...)
	return &clone
}

func cloneProjectionDevice(src *OverlayProjectionDevice) *OverlayProjectionDevice {
	if src == nil {
		return nil
	}
	clone := *src
	clone.Device = *cloneOverlayDevice(&src.Device)
	clone.Tags = append([]string(nil), src.Tags...)
	clone.Attachments = append([]string(nil), src.Attachments...)
	return &clone
}

func (s *memoryStore) GetOverlayNode(_ context.Context, nodeID string) (*OverlayNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	node := s.overlayNodes[strings.TrimSpace(nodeID)]
	if node == nil {
		return nil, ErrOverlayNodeNotFound
	}
	return cloneOverlayNode(node), nil
}

func (s *memoryStore) CreateOverlayNodeCredential(_ context.Context, credential *OverlayNodeCredential, audit *AuditLog) error {
	if credential == nil || credential.ID == "" || credential.NodeID == "" || len(credential.TokenHash) != sha256.Size || credential.ExpiresAt.IsZero() {
		return errors.New("valid overlay node credential is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overlayNodes[credential.NodeID] == nil {
		return ErrOverlayNodeNotFound
	}
	hash := string(credential.TokenHash)
	if s.overlayNodeCredentials[credential.ID] != nil || s.overlayNodeCredentialHashes[hash] != "" {
		return errors.New("overlay node credential already exists")
	}
	stored := cloneOverlayNodeCredential(credential)
	stored.CreatedAt = time.Now().UTC()
	s.overlayNodeCredentials[stored.ID] = stored
	s.overlayNodeCredentialHashes[hash] = stored.ID
	audit.CreatedAt = stored.CreatedAt
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	*credential = *cloneOverlayNodeCredential(stored)
	return nil
}

func (s *memoryStore) RevokeOverlayNodeCredential(_ context.Context, nodeID, credentialID string, revokedAt time.Time, audit *AuditLog) error {
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	credential := s.overlayNodeCredentials[strings.TrimSpace(credentialID)]
	if credential == nil || credential.NodeID != strings.TrimSpace(nodeID) {
		return ErrOverlayNodeCredentialNotFound
	}
	if credential.RevokedAt == nil {
		value := revokedAt.UTC()
		credential.RevokedAt = &value
	}
	audit.CreatedAt = revokedAt.UTC()
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return nil
}

func (s *memoryStore) AuthenticateOverlayNodeCredential(_ context.Context, tokenHash []byte, now time.Time) (*OverlayNodeCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.overlayNodeCredentialHashes[string(tokenHash)]
	credential := s.overlayNodeCredentials[id]
	if credential == nil {
		return nil, ErrOverlayNodeCredentialNotFound
	}
	if credential.RevokedAt != nil {
		return nil, ErrOverlayNodeCredentialRevoked
	}
	if !credential.ExpiresAt.After(now.UTC()) {
		return nil, ErrOverlayNodeCredentialExpired
	}
	used := now.UTC()
	credential.LastUsedAt = &used
	return cloneOverlayNodeCredential(credential), nil
}

func (s *memoryStore) RecordOverlayGatewayHeartbeat(_ context.Context, heartbeat *OverlayGatewayHeartbeat) error {
	if heartbeat == nil || heartbeat.NodeID == "" {
		return errors.New("valid overlay gateway heartbeat is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overlayNodes[heartbeat.NodeID] == nil {
		return ErrOverlayNodeNotFound
	}
	current := s.overlayGatewayHeartbeats[heartbeat.NodeID]
	if current != nil && (heartbeat.ObservedGeneration < current.ObservedGeneration || heartbeat.AppliedGeneration < current.AppliedGeneration) {
		return ErrOverlayGatewayReportStale
	}
	clone := *heartbeat
	clone.ReceivedAt = time.Now().UTC()
	s.overlayGatewayHeartbeats[heartbeat.NodeID] = &clone
	*heartbeat = clone
	return nil
}

func (s *memoryStore) GetLatestOverlayGatewaySnapshot(_ context.Context, nodeID string) (*OverlayGatewaySnapshotRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := s.overlayGatewaySnapshots[strings.TrimSpace(nodeID)]
	if len(records) == 0 {
		return nil, ErrOverlayGatewaySnapshotNotFound
	}
	return cloneOverlayGatewaySnapshot(records[len(records)-1]), nil
}

func (s *memoryStore) SaveOverlayGatewaySnapshot(_ context.Context, record *OverlayGatewaySnapshotRecord) error {
	if record == nil || record.NodeID == "" || record.Generation == 0 || record.SourceRevision == "" || len(record.SignedPayload) == 0 {
		return errors.New("valid overlay gateway snapshot is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.overlayGatewaySnapshots[record.NodeID]
	if len(records) > 0 {
		latest := records[len(records)-1]
		if latest.SourceRevision == record.SourceRevision {
			return ErrOverlayGatewaySourceExists
		}
		if record.Generation != latest.Generation+1 || record.ExpectedPreviousGeneration != latest.Generation {
			return ErrOverlayGatewayGenerationStale
		}
	} else if record.Generation != 1 || record.ExpectedPreviousGeneration != 0 {
		return ErrOverlayGatewayGenerationStale
	}
	stored := cloneOverlayGatewaySnapshot(record)
	stored.CreatedAt = time.Now().UTC()
	s.overlayGatewaySnapshots[record.NodeID] = append(records, stored)
	*record = *cloneOverlayGatewaySnapshot(stored)
	return nil
}

func (s *memoryStore) RecordOverlayGatewayApplyResult(_ context.Context, result *OverlayGatewayApplyResult) (bool, error) {
	if result == nil || result.NodeID == "" || result.SnapshotID == "" {
		return false, errors.New("valid overlay gateway apply result is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.overlayGatewaySnapshots[result.NodeID]
	var snapshot *OverlayGatewaySnapshotRecord
	for _, candidate := range records {
		if candidate.SnapshotID == result.SnapshotID {
			snapshot = candidate
			break
		}
	}
	if snapshot == nil || snapshot.Generation != result.ObservedGeneration {
		return false, ErrOverlayGatewayReportStale
	}
	key := result.NodeID + "\x00" + result.SnapshotID
	if previous := s.overlayGatewayResults[key]; previous != nil {
		if previous.ObservedGeneration == result.ObservedGeneration && previous.AppliedGeneration == result.AppliedGeneration && previous.RuntimeApplied == result.RuntimeApplied && previous.Result == result.Result && bytes.Equal(previous.Diff, result.Diff) {
			*result = *previous
			result.Diff = append([]byte(nil), previous.Diff...)
			return true, nil
		}
		if strings.HasPrefix(previous.Result, "apply_") && previous.Result != "apply_failed_rollback_failed" && result.Result == "applied" && result.RuntimeApplied && result.AppliedGeneration == result.ObservedGeneration {
			clone := *result
			clone.Diff = append([]byte(nil), result.Diff...)
			clone.ReceivedAt = time.Now().UTC()
			s.overlayGatewayResults[key] = &clone
			s.overlayGatewayAttempts = append(s.overlayGatewayAttempts, clone)
			*result = clone
			return false, nil
		}
		return false, ErrOverlayGatewayReportStale
	}
	clone := *result
	clone.Diff = append([]byte(nil), result.Diff...)
	clone.ReceivedAt = time.Now().UTC()
	s.overlayGatewayResults[key] = &clone
	if strings.HasPrefix(clone.Result, "apply_") || clone.Result == "applied" {
		s.overlayGatewayAttempts = append(s.overlayGatewayAttempts, clone)
	}
	*result = clone
	return false, nil
}

func (s *memoryStore) IsOverlayGatewayGenerationApplied(_ context.Context, nodeID string, generation uint64) (bool, error) {
	if generation == 0 {
		return true, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, result := range s.overlayGatewayResults {
		if result.NodeID == strings.TrimSpace(nodeID) && result.ObservedGeneration == generation && result.AppliedGeneration == generation && result.RuntimeApplied && result.Result == "applied" {
			return true, nil
		}
	}
	return false, nil
}

func (s *memoryStore) ListOverlayProjectionDevicesByNetwork(_ context.Context, networkID string) ([]OverlayProjectionDevice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]OverlayProjectionDevice, 0)
	for key, device := range s.overlayDevices {
		if device.NetworkID != strings.TrimSpace(networkID) {
			continue
		}
		if metadata := s.overlayProjectionDevices[key]; metadata != nil {
			result = append(result, *cloneProjectionDevice(metadata))
		} else {
			result = append(result, OverlayProjectionDevice{Device: *cloneOverlayDevice(device)})
		}
	}
	sortProjectionDevices(result)
	return result, nil
}

func sortProjectionDevices(values []OverlayProjectionDevice) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && (values[j].Device.ID < values[j-1].Device.ID || (values[j].Device.ID == values[j-1].Device.ID && values[j].Device.UserID < values[j-1].Device.UserID)); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (s *memoryStore) ImportOverlayStaticClients(_ context.Context, input *OverlayStaticImport, audit *AuditLog) (*OverlayStaticImportReceipt, bool, error) {
	if input == nil || !strings.HasPrefix(input.IdempotencyKey, "sha256-") || len(input.IdempotencyKey) != 71 || len(input.BodySHA256) != 64 || input.OwnerUserID == "" || input.NetworkID == "" || len(input.Devices) == 0 {
		return nil, false, errors.New("valid overlay static import is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if receipt := s.overlayStaticImports[input.IdempotencyKey]; receipt != nil {
		if s.overlayStaticImportHashes[input.IdempotencyKey] != input.BodySHA256 {
			return nil, false, ErrOverlayStaticImportIdempotency
		}
		clone := *receipt
		return &clone, true, nil
	}
	if s.byID[input.OwnerUserID] == nil {
		return nil, false, ErrUserNotFound
	}
	for _, projected := range input.Devices {
		for _, existing := range s.overlayDevices {
			identityMismatch := existing.ID == projected.Device.ID && (existing.UserID != input.OwnerUserID || existing.NetworkID != input.NetworkID || existing.WireGuardPublicKey != projected.Device.WireGuardPublicKey || existing.WireGuardAddress != projected.Device.WireGuardAddress)
			networkCollision := existing.NetworkID == input.NetworkID && existing.ID != projected.Device.ID && (existing.WireGuardPublicKey == projected.Device.WireGuardPublicKey || existing.WireGuardAddress == projected.Device.WireGuardAddress)
			if identityMismatch || networkCollision {
				return nil, false, ErrOverlayStaticImportConflict
			}
		}
	}
	now := time.Now().UTC()
	for _, projected := range input.Devices {
		device := cloneOverlayDevice(&projected.Device)
		device.UserID, device.NetworkID = input.OwnerUserID, input.NetworkID
		if device.Status == "" {
			device.Status = OverlayDeviceActive
			device.StateVersion = 1
			device.KeyVersion = 1
		}
		if device.Name == "" {
			device.Name = device.ID
		}
		key := overlayDeviceKey(device.UserID, device.ID)
		if previous := s.overlayDevices[key]; previous != nil {
			device.CreatedAt = previous.CreatedAt
		} else {
			device.CreatedAt = now
		}
		device.UpdatedAt = now
		s.overlayDevices[key] = device
		metadata := cloneProjectionDevice(&projected)
		metadata.Device = *cloneOverlayDevice(device)
		s.overlayProjectionDevices[key] = metadata
	}
	receipt := &OverlayStaticImportReceipt{ImportID: "import_" + strings.TrimPrefix(input.BodySHA256, "sha256:")[:24], IdempotencyKey: input.IdempotencyKey, OwnerUserID: input.OwnerUserID, NetworkID: input.NetworkID, BaselineSHA256: input.BaselineSHA256, DeviceCount: len(input.Devices), CreatedAt: now}
	s.overlayStaticImports[input.IdempotencyKey] = receipt
	s.overlayStaticImportHashes[input.IdempotencyKey] = input.BodySHA256
	audit.CreatedAt = now
	if audit.Details == nil {
		audit.Details = map[string]any{}
	}
	audit.Details["import_id"] = receipt.ImportID
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	clone := *receipt
	return &clone, false, nil
}
