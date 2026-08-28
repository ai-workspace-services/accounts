package gatewayprojection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"account/internal/overlay/acl"
	"account/internal/overlay/domain"
	"account/internal/overlay/projection"
	"account/internal/store"
)

var ErrEmptyPeerProjection = errors.New("empty gateway peer projection requires explicit override")

type Config struct {
	Lifetime                   time.Duration
	RenewalInterval            time.Duration
	InterfaceName              string
	WireGuardListenPort        int
	RelayListenHost            string
	PersistentKeepaliveSeconds int
	AllowEmptyPeers            bool
	MaxPeerRemovalPercent      float64
}

type Service struct {
	store   store.Store
	signing *projection.Service
	clock   func() time.Time
	config  Config
}

func NewService(st store.Store, signing *projection.Service, clock func() time.Time, config Config) (*Service, error) {
	if st == nil || signing == nil {
		return nil, errors.New("gateway projection store and signing service are required")
	}
	if clock == nil {
		clock = time.Now
	}
	if config.Lifetime <= 0 {
		config.Lifetime = 30 * time.Minute
	}
	if config.RenewalInterval <= 0 || config.RenewalInterval >= config.Lifetime {
		config.RenewalInterval = config.Lifetime / 2
	}
	if config.InterfaceName == "" {
		config.InterfaceName = "wg-xco"
	}
	if config.WireGuardListenPort == 0 {
		config.WireGuardListenPort = 51820
	}
	if config.RelayListenHost == "" {
		config.RelayListenHost = "0.0.0.0"
	}
	if config.PersistentKeepaliveSeconds == 0 {
		config.PersistentKeepaliveSeconds = 25
	}
	if config.MaxPeerRemovalPercent < 0 || config.MaxPeerRemovalPercent > 100 {
		return nil, errors.New("gateway peer removal percentage must be between 0 and 100")
	}
	return &Service{store: st, signing: signing, clock: clock, config: config}, nil
}

type sourceDocument struct {
	Version      int                  `json:"version"`
	RenewalEpoch int64                `json:"renewal_epoch"`
	Node         sourceNode           `json:"node"`
	Peers        []domain.GatewayPeer `json:"peers"`
	Safety       domain.GatewaySafety `json:"safety"`
	Policy       string               `json:"policy"`
}

type sourceNode struct {
	ID, NetworkID, Address, Host string
	Port                         int
}

func (s *Service) Project(ctx context.Context, nodeID string) (domain.GatewaySnapshot, error) {
	for attempt := 0; attempt < 32; attempt++ {
		now := s.clock().UTC().Truncate(time.Second)
		node, err := s.store.GetOverlayNode(ctx, nodeID)
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		if !node.Healthy {
			return domain.GatewaySnapshot{}, errors.New("gateway node is not healthy")
		}
		devices, err := s.store.ListOverlayProjectionDevicesByNetwork(ctx, node.NetworkID)
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		users, err := s.store.ListUsers(ctx)
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		eligible := map[string]bool{}
		for _, user := range users {
			if user.Active {
				eligible[user.ID] = true
			}
		}
		peers := make([]domain.GatewayPeer, 0, len(devices))
		seenIDs, seenKeys, seenAddresses := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, item := range devices {
			if !eligible[item.Device.UserID] {
				continue
			}
			if !attachedToNode(item.Attachments, node.ID, node.EndpointHost) {
				continue
			}
			if seenIDs[item.Device.ID] || seenKeys[item.Device.WireGuardPublicKey] || seenAddresses[item.Device.WireGuardAddress] {
				return domain.GatewaySnapshot{}, errors.New("gateway projection contains a duplicate network device identity, public key, or address")
			}
			seenIDs[item.Device.ID], seenKeys[item.Device.WireGuardPublicKey], seenAddresses[item.Device.WireGuardAddress] = true, true, true
			peers = append(peers, domain.GatewayPeer{DeviceID: item.Device.ID, PublicKey: item.Device.WireGuardPublicKey, AllowedIPs: []string{item.Device.WireGuardAddress}, PersistentKeepaliveSeconds: s.config.PersistentKeepaliveSeconds})
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].DeviceID < peers[j].DeviceID })
		if len(peers) == 0 && !s.config.AllowEmptyPeers {
			return domain.GatewaySnapshot{}, ErrEmptyPeerProjection
		}
		safety := domain.GatewaySafety{AllowEmptyPeers: s.config.AllowEmptyPeers, MaxPeerRemovalPercent: s.config.MaxPeerRemovalPercent}
		policy, err := acl.ResolveActive(ctx, s.store, node.NetworkID)
		if err != nil {
			return domain.GatewaySnapshot{}, fmt.Errorf("resolve active gateway policy: %w", err)
		}
		source := sourceDocument{Version: 1, RenewalEpoch: now.Unix() / int64(s.config.RenewalInterval/time.Second), Node: sourceNode{ID: node.ID, NetworkID: node.NetworkID, Address: node.WireGuardAddress, Host: node.EndpointHost, Port: node.EndpointPort}, Peers: peers, Safety: safety, Policy: fmt.Sprintf("%d:%s", policy.Generation, policy.Digest)}
		sourceRaw, _ := json.Marshal(source)
		sourceSum := sha256.Sum256(sourceRaw)
		sourceRevision := "sha256:" + hex.EncodeToString(sourceSum[:])
		latest, exists, err := s.latest(ctx, node.ID)
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		if exists && latest.SourceRevision == sourceRevision && latest.ExpiresAt.After(now) {
			return decodeRecord(latest)
		}
		generation := uint64(1)
		previousGeneration := uint64(0)
		var previous domain.GatewaySnapshot
		if exists {
			generation = latest.Generation + 1
			previousGeneration = latest.Generation
			previous, err = decodeRecord(latest)
			if err != nil {
				return domain.GatewaySnapshot{}, err
			}
		}
		snapshot := domain.GatewaySnapshot{SchemaVersion: 1, SnapshotID: deriveSnapshotID(node.ID, generation, sourceRevision), NodeID: node.ID, Generation: generation, ExpectedPreviousGeneration: previousGeneration, IssuedAt: now, ExpiresAt: now.Add(s.config.Lifetime).UTC().Truncate(time.Second), ProxyCore: domain.ProxyCoreXray, Safety: safety, WireGuard: domain.GatewayWireGuard{InterfaceName: s.config.InterfaceName, ListenPort: s.config.WireGuardListenPort, Addresses: []string{node.WireGuardAddress}, Peers: peers}, Relay: domain.GatewayRelay{Transport: "vless-tls-xudp", ListenHost: s.config.RelayListenHost, ListenPort: node.EndpointPort, ServerNames: []string{node.EndpointHost}, CredentialRefs: []string{relayCredentialRef(node.ID)}}, Policy: domain.GatewayPolicy{Generation: policy.Generation, Backend: "nftables", RulesetSHA256: policy.Digest}}
		payload, err := snapshot.SigningBytes()
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		snapshot.Signature, err = s.signing.SignControlPlanePayload(payload)
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		if err := s.signing.VerifyControlPlanePayload(payload, snapshot.Signature); err != nil {
			return domain.GatewaySnapshot{}, fmt.Errorf("self-verify gateway snapshot: %w", err)
		}
		if exists {
			if err := snapshot.ValidateTransition(previous); err != nil {
				return domain.GatewaySnapshot{}, err
			}
		} else if err := snapshot.Validate(); err != nil {
			return domain.GatewaySnapshot{}, err
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		if _, err := domain.DecodeGatewaySnapshot(raw); err != nil {
			return domain.GatewaySnapshot{}, fmt.Errorf("strict gateway snapshot validation: %w", err)
		}
		record := &store.OverlayGatewaySnapshotRecord{NodeID: node.ID, SnapshotID: snapshot.SnapshotID, Generation: generation, ExpectedPreviousGeneration: previousGeneration, SourceRevision: sourceRevision, SigningKeyID: snapshot.Signature.KeyID, SignedPayload: raw, IssuedAt: snapshot.IssuedAt, ExpiresAt: snapshot.ExpiresAt}
		err = s.store.SaveOverlayGatewaySnapshot(ctx, record)
		if errors.Is(err, store.ErrOverlayGatewayGenerationStale) || errors.Is(err, store.ErrOverlayGatewaySourceExists) {
			continue
		}
		if err != nil {
			return domain.GatewaySnapshot{}, err
		}
		return snapshot, nil
	}
	return domain.GatewaySnapshot{}, errors.New("gateway projection generation contention")
}

func (s *Service) latest(ctx context.Context, nodeID string) (*store.OverlayGatewaySnapshotRecord, bool, error) {
	record, err := s.store.GetLatestOverlayGatewaySnapshot(ctx, nodeID)
	if errors.Is(err, store.ErrOverlayGatewaySnapshotNotFound) {
		return nil, false, nil
	}
	return record, err == nil, err
}
func decodeRecord(record *store.OverlayGatewaySnapshotRecord) (domain.GatewaySnapshot, error) {
	snapshot, err := domain.DecodeGatewaySnapshot(record.SignedPayload)
	if err != nil {
		return domain.GatewaySnapshot{}, fmt.Errorf("decode stored gateway snapshot: %w", err)
	}
	return snapshot, nil
}
func deriveSnapshotID(nodeID string, generation uint64, source string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", nodeID, generation, source)))
	return "snap_" + hex.EncodeToString(sum[:12])
}
func relayCredentialRef(nodeID string) string {
	return "relay_credential_" + strings.ReplaceAll(nodeID, ":", "_")
}
func attachedToNode(attachments []string, nodeID, endpoint string) bool {
	if len(attachments) == 0 {
		return true
	}
	for _, value := range attachments {
		if value == nodeID || value == endpoint {
			return true
		}
	}
	return false
}
