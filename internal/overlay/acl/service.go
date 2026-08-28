package acl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"account/internal/store"
)

type Service struct{ store store.Store }

func NewService(st store.Store) (*Service, error) {
	if st == nil {
		return nil, errors.New("ACL store is required")
	}
	return &Service{store: st}, nil
}

func (s *Service) inventory(ctx context.Context, networkID string) (Inventory, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return Inventory{}, err
	}
	devices, err := s.store.ListOverlayProjectionDevicesByNetwork(ctx, networkID)
	if err != nil {
		return Inventory{}, err
	}
	inv := Inventory{Users: map[string]string{}, Devices: map[string][]string{}, DeviceOwners: map[string]string{}, EligibleDevices: map[string]bool{}}
	emails := map[string]string{}
	for _, u := range users {
		email := strings.ToLower(strings.TrimSpace(u.Email))
		inv.Users[email] = u.ID
		if u.Active {
			emails[u.ID] = email
		}
	}
	for _, d := range devices {
		inv.Devices[d.Device.ID] = append([]string(nil), d.Tags...)
		if d.Device.Status != "" && d.Device.Status != store.OverlayDeviceActive {
			continue
		}
		if emails[d.Device.UserID] == "" {
			continue
		}
		inv.EligibleDevices[d.Device.ID] = true
		if email := emails[d.Device.UserID]; email != "" {
			inv.DeviceOwners[d.Device.ID] = d.Device.UserID
		}
	}
	return inv, nil
}
func (s *Service) Validate(ctx context.Context, networkID string, raw []byte, contentType string) (Build, Document, error) {
	doc, err := Parse(raw, contentType)
	if err != nil {
		return Build{}, Document{}, err
	}
	inv, err := s.inventory(ctx, networkID)
	if err != nil {
		return Build{}, Document{}, err
	}
	build, err := Compile(doc, networkID, 1, inv)
	return build, doc, err
}
func (s *Service) Create(ctx context.Context, networkID, owner string, raw []byte, contentType string) (*store.OverlayPolicyRevision, error) {
	doc, err := Parse(raw, contentType)
	if err != nil {
		return nil, err
	}
	inv, err := s.inventory(ctx, networkID)
	if err != nil {
		return nil, err
	}
	source, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		revision := uint64(1)
		latest, lookup := s.store.GetLatestOverlayPolicyRevision(ctx, networkID)
		if lookup == nil {
			revision = latest.Revision + 1
		} else if !errors.Is(lookup, store.ErrOverlayPolicyNotFound) {
			return nil, lookup
		}
		build, err := Compile(doc, networkID, revision, inv)
		if err != nil {
			return nil, err
		}
		warnings, _ := json.Marshal(build.Warnings)
		record := &store.OverlayPolicyRevision{NetworkID: networkID, Revision: revision, OwnerUserID: owner, Name: doc.Metadata.Name, Source: source, Artifact: build.Canonical, ArtifactSHA256: build.Digest, CompilerVersion: CompilerVersion, Warnings: warnings}
		audit := &store.AuditLog{Action: store.AuditActionOverlayPolicyCreate, ActorUUID: owner, Details: map[string]any{"target_uuid": owner, "network_id": networkID, "revision": revision, "artifact_sha256": build.Digest}}
		if err = s.store.CreateOverlayPolicyRevision(ctx, record, audit); errors.Is(err, store.ErrOverlayPolicyConflict) {
			continue
		} else if err != nil {
			return nil, err
		}
		return record, nil
	}
	return nil, errors.New("policy revision contention")
}
func (s *Service) Get(ctx context.Context, networkID string, revision uint64, owner string) (*store.OverlayPolicyRevision, error) {
	p, err := s.store.GetOverlayPolicyRevision(ctx, networkID, revision)
	if err != nil {
		return nil, err
	}
	_ = owner
	return p, nil
}
func (s *Service) Activate(ctx context.Context, networkID string, revision uint64, owner string) (*store.OverlayPolicyRevision, error) {
	audit := &store.AuditLog{Action: store.AuditActionOverlayPolicyActivate, ActorUUID: owner, Details: map[string]any{"target_uuid": owner, "network_id": networkID, "revision": revision}}
	return s.store.ActivateOverlayPolicyRevision(ctx, networkID, revision, owner, audit)
}
func (s *Service) Explain(ctx context.Context, networkID string, revision uint64, owner string, q Query) (Explanation, error) {
	p, err := s.Get(ctx, networkID, revision, owner)
	if err != nil {
		return Explanation{}, err
	}
	var doc Document
	if err = json.Unmarshal(p.Source, &doc); err != nil {
		return Explanation{}, fmt.Errorf("decode policy source: %w", err)
	}
	inv, err := s.inventory(ctx, networkID)
	if err != nil {
		return Explanation{}, err
	}
	build, err := Compile(doc, networkID, revision, inv)
	if err != nil {
		return Explanation{}, err
	}
	return Explain(build.Artifact, q), nil
}
func (s *Service) UpdateDeviceTags(ctx context.Context, networkID, deviceID, actorUserID, actorEmail string, next []string) error {
	p, err := s.store.GetActiveOverlayPolicy(ctx, networkID)
	if err != nil {
		return err
	}
	var doc Document
	if err = json.Unmarshal(p.Source, &doc); err != nil {
		return err
	}
	inv, err := s.inventory(ctx, networkID)
	if err != nil {
		return err
	}
	ownerID := inv.DeviceOwners[deviceID]
	if ownerID == "" {
		return store.ErrOverlayJoinConstraint
	}
	build, err := Compile(doc, networkID, p.Revision, inv)
	if err != nil {
		return err
	}
	current := inv.Devices[deviceID]
	changed := symmetricDifference(current, uniqueSorted(next))
	for _, tag := range changed {
		tag = strings.ToLower(tag)
		if !CanAssignTag(build.Artifact, "user:"+strings.ToLower(strings.TrimSpace(actorEmail)), tag) {
			return store.ErrOverlayJoinConstraint
		}
	}
	audit := &store.AuditLog{Action: store.AuditActionOverlayDeviceTagsUpdate, ActorUUID: actorUserID, Details: map[string]any{"network_id": networkID, "device_id": deviceID, "tags": uniqueSorted(next)}}
	return s.store.UpdateOverlayDeviceTags(ctx, actorUserID, networkID, deviceID, uniqueSorted(next), audit)
}
func symmetricDifference(a, b []string) []string {
	set := map[string]int{}
	for _, v := range a {
		set[strings.ToLower(strings.TrimSpace(v))]++
	}
	for _, v := range b {
		set[strings.ToLower(strings.TrimSpace(v))]++
	}
	out := []string{}
	for v, count := range set {
		if count == 1 {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

type ActiveArtifact struct {
	Generation      uint64
	Digest          string
	Canonical       []byte
	CompilerVersion string
}

func ResolveActive(ctx context.Context, st store.Store, networkID string) (ActiveArtifact, error) {
	p, err := st.GetActiveOverlayPolicy(ctx, networkID)
	if errors.Is(err, store.ErrOverlayPolicyNotFound) {
		a := EnforcementArtifact{SchemaVersion: 1, CompilerVersion: CompilerVersion, NetworkID: networkID, Revision: 0, DefaultAction: "deny", ProtectedFlows: []string{"control:controller-session", "control:gateway-apply-result", "control:gateway-heartbeat", "control:gateway-policy-artifact", "control:gateway-snapshot"}, Rules: []EnforcementRule{}}
		raw, _ := json.Marshal(a)
		sum := sha256Hex(raw)
		return ActiveArtifact{Generation: 1, Digest: sum, Canonical: raw, CompilerVersion: CompilerVersion}, nil
	}
	if err != nil {
		return ActiveArtifact{}, err
	}
	var doc Document
	if err = json.Unmarshal(p.Source, &doc); err != nil {
		return ActiveArtifact{}, fmt.Errorf("decode active policy source: %w", err)
	}
	service, _ := NewService(st)
	inv, err := service.inventory(ctx, networkID)
	if err != nil {
		return ActiveArtifact{}, fmt.Errorf("resolve policy inventory: %w", err)
	}
	build, err := Compile(doc, networkID, p.Revision, inv)
	if err != nil {
		return ActiveArtifact{}, fmt.Errorf("recompile active policy: %w", err)
	}
	if len(p.Artifact) > 0 && sha256Hex(p.Artifact) != p.ArtifactSHA256 {
		return ActiveArtifact{}, errors.New("stored active policy artifact digest mismatch")
	}
	if len(p.Artifact) == 0 || build.Digest != p.ArtifactSHA256 {
		audit := &store.AuditLog{Action: store.AuditActionOverlayPolicyRecompile, ActorUUID: "system", Details: map[string]any{"network_id": networkID, "revision": p.Revision, "previous_digest": p.ArtifactSHA256, "artifact_sha256": build.Digest}}
		refreshed, _, refreshErr := st.RefreshOverlayPolicyBuild(ctx, networkID, p.Revision, p.ArtifactSHA256, build.Canonical, build.Digest, CompilerVersion, audit)
		if refreshErr == nil {
			p = refreshed
		} else if errors.Is(refreshErr, store.ErrOverlayPolicyConflict) {
			p, err = st.GetActiveOverlayPolicy(ctx, networkID)
			if err != nil {
				return ActiveArtifact{}, err
			}
		} else {
			return ActiveArtifact{}, fmt.Errorf("persist policy recompile: %w", refreshErr)
		}
	}
	if p.Generation == 0 || len(p.Artifact) == 0 {
		return ActiveArtifact{}, errors.New("active policy is invalid")
	}
	return ActiveArtifact{Generation: p.Generation, Digest: p.ArtifactSHA256, Canonical: append([]byte(nil), p.Artifact...), CompilerVersion: p.CompilerVersion}, nil
}
func sha256Hex(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
