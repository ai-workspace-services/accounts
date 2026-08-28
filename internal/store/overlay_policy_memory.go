package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

func cloneOverlayPolicy(in *OverlayPolicyRevision) *OverlayPolicyRevision {
	if in == nil {
		return nil
	}
	out := *in
	out.Source = append([]byte(nil), in.Source...)
	out.Artifact = append([]byte(nil), in.Artifact...)
	out.Warnings = append([]byte(nil), in.Warnings...)
	if in.ActivatedAt != nil {
		v := in.ActivatedAt.UTC()
		out.ActivatedAt = &v
	}
	return &out
}
func validOverlayPolicy(p *OverlayPolicyRevision) bool {
	return p != nil && p.Revision > 0 && strings.TrimSpace(p.NetworkID) != "" && strings.TrimSpace(p.OwnerUserID) != "" && len(p.Source) > 0 && len(p.Artifact) > 0 && len(p.ArtifactSHA256) == 64 && p.CompilerVersion != ""
}
func (s *memoryStore) CreateOverlayPolicyRevision(_ context.Context, p *OverlayPolicyRevision, audit *AuditLog) error {
	if !validOverlayPolicy(p) {
		return errors.New("valid overlay policy revision is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.overlayPolicies[p.NetworkID]
	if p.Revision != uint64(len(records)+1) {
		return ErrOverlayPolicyConflict
	}
	p.Status = "draft"
	p.Generation = 0
	p.CreatedAt = time.Now().UTC()
	p.ValidatedAt = p.CreatedAt
	stored := cloneOverlayPolicy(p)
	s.overlayPolicies[p.NetworkID] = append(records, stored)
	audit.CreatedAt = p.CreatedAt
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	*p = *cloneOverlayPolicy(stored)
	return nil
}
func (s *memoryStore) GetOverlayPolicyRevision(_ context.Context, networkID string, revision uint64) (*OverlayPolicyRevision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := s.overlayPolicies[strings.TrimSpace(networkID)]
	if revision == 0 || int(revision) > len(records) {
		return nil, ErrOverlayPolicyNotFound
	}
	return cloneOverlayPolicy(records[revision-1]), nil
}
func (s *memoryStore) GetLatestOverlayPolicyRevision(_ context.Context, networkID string) (*OverlayPolicyRevision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := s.overlayPolicies[strings.TrimSpace(networkID)]
	if len(records) == 0 {
		return nil, ErrOverlayPolicyNotFound
	}
	return cloneOverlayPolicy(records[len(records)-1]), nil
}
func (s *memoryStore) ActivateOverlayPolicyRevision(_ context.Context, networkID string, revision uint64, actor string, audit *AuditLog) (*OverlayPolicyRevision, error) {
	if strings.TrimSpace(actor) == "" {
		return nil, ErrOverlayPolicyConflict
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.overlayPolicies[networkID]
	if revision == 0 || int(revision) > len(records) {
		return nil, ErrOverlayPolicyNotFound
	}
	p := records[revision-1]
	_ = actor
	if p.Status == "active" {
		return cloneOverlayPolicy(p), nil
	}
	if old := s.overlayActivePolicies[networkID]; old > 0 {
		records[old-1].Status = "superseded"
	}
	generation := uint64(1)
	for _, record := range records {
		if record.Generation >= generation {
			generation = record.Generation + 1
		}
	}
	now := time.Now().UTC()
	p.Status = "active"
	p.Generation = generation
	p.ActivatedAt = &now
	s.overlayActivePolicies[networkID] = revision
	audit.CreatedAt = now
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return cloneOverlayPolicy(p), nil
}
func (s *memoryStore) GetActiveOverlayPolicy(_ context.Context, networkID string) (*OverlayPolicyRevision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	revision := s.overlayActivePolicies[strings.TrimSpace(networkID)]
	if revision == 0 {
		return nil, ErrOverlayPolicyNotFound
	}
	return cloneOverlayPolicy(s.overlayPolicies[networkID][revision-1]), nil
}
func (s *memoryStore) RefreshOverlayPolicyBuild(_ context.Context, networkID string, revision uint64, expected string, artifact []byte, digest, compiler string, audit *AuditLog) (*OverlayPolicyRevision, bool, error) {
	if len(artifact) == 0 || len(digest) != 64 || compiler == "" {
		return nil, false, ErrOverlayPolicyConflict
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overlayActivePolicies[networkID] != revision {
		return nil, false, ErrOverlayPolicyConflict
	}
	p := s.overlayPolicies[networkID][revision-1]
	if p.ArtifactSHA256 == digest {
		return cloneOverlayPolicy(p), false, nil
	}
	if p.ArtifactSHA256 != expected {
		return nil, false, ErrOverlayPolicyConflict
	}
	generation := uint64(1)
	for _, record := range s.overlayPolicies[networkID] {
		if record.Generation >= generation {
			generation = record.Generation + 1
		}
	}
	p.Artifact = append([]byte(nil), artifact...)
	p.ArtifactSHA256 = digest
	p.CompilerVersion = compiler
	p.Generation = generation
	now := time.Now().UTC()
	p.ValidatedAt = now
	audit.CreatedAt = now
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return cloneOverlayPolicy(p), true, nil
}
func (s *memoryStore) UpdateOverlayDeviceTags(_ context.Context, userID, networkID, deviceID string, tags []string, audit *AuditLog) error {
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = userID
	key := ""
	var device *OverlayDevice
	for candidate, value := range s.overlayDevices {
		if value.ID == deviceID && value.NetworkID == networkID {
			key, device = candidate, value
			break
		}
	}
	if device == nil {
		return ErrOverlayJoinConstraint
	}
	metadata := s.overlayProjectionDevices[key]
	if metadata == nil {
		metadata = &OverlayProjectionDevice{Device: *cloneOverlayDevice(device)}
	}
	metadata.Tags = cloneStringSlice(tags)
	s.overlayProjectionDevices[key] = metadata
	audit.CreatedAt = time.Now().UTC()
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return nil
}
