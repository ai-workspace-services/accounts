// Package domain contains transport-neutral XConnect-One overlay contracts.
// It deliberately contains no HTTP, persistence, or platform runtime code so
// the controller, generated clients, and gateway agent can share compatibility
// and safety rules.
package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

const (
	SchemaVersionV1  = 1
	ProxyCoreXray    = "xray"
	TransportVLESS   = "vless-tls-xudp"
	SignatureEd25519 = "Ed25519"
	PolicyNFTables   = "nftables"
	LoopbackHost     = "127.0.0.1"
)

var (
	idPattern        = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{2,127}$`)
	interfacePattern = regexp.MustCompile(`^[a-zA-Z0-9_=+.-]{1,15}$`)
	forbiddenFields  = map[string]struct{}{
		"private_key":   {},
		"refresh_token": {},
		"vault_token":   {},
	}
)

type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

func (s Signature) Validate() error {
	if s.Algorithm != SignatureEd25519 {
		return fmt.Errorf("signature algorithm must be %q", SignatureEd25519)
	}
	if err := validateID("signature key_id", s.KeyID); err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(s.Value)
	if err != nil || len(decoded) != 64 {
		return errors.New("signature value must be a base64-encoded 64-byte Ed25519 signature")
	}
	return nil
}

type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type RemoteEndpoint struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ServerName string `json:"server_name"`
}

type ClientTransport struct {
	Kind     string         `json:"kind"`
	Loopback Endpoint       `json:"loopback"`
	Remote   RemoteEndpoint `json:"remote"`
	AuthID   string         `json:"auth_id"`
}

func (t ClientTransport) Validate() error {
	if t.Kind != TransportVLESS {
		return fmt.Errorf("transport kind must be %q", TransportVLESS)
	}
	if t.Loopback.Host != LoopbackHost {
		return fmt.Errorf("transport loopback host must be %q", LoopbackHost)
	}
	if err := validatePort("transport loopback port", t.Loopback.Port); err != nil {
		return err
	}
	if err := validateHost("transport remote host", t.Remote.Host); err != nil {
		return err
	}
	if err := validatePort("transport remote port", t.Remote.Port); err != nil {
		return err
	}
	if err := validateHost("transport remote server_name", t.Remote.ServerName); err != nil {
		return err
	}
	return validateID("transport auth_id", t.AuthID)
}

type ClientPeer struct {
	GatewayID                  string   `json:"gateway_id"`
	PublicKey                  string   `json:"public_key"`
	AllowedIPs                 []string `json:"allowed_ips"`
	Endpoint                   Endpoint `json:"endpoint"`
	PersistentKeepaliveSeconds int      `json:"persistent_keepalive_seconds,omitempty"`
}

type ClientWireGuard struct {
	InterfaceName string       `json:"interface_name"`
	Addresses     []string     `json:"addresses"`
	MTU           int          `json:"mtu"`
	Peers         []ClientPeer `json:"peers"`
}

type SignedConfig struct {
	SchemaVersion int             `json:"schema_version"`
	ConfigID      string          `json:"config_id"`
	NetworkID     string          `json:"network_id"`
	DeviceID      string          `json:"device_id"`
	Generation    uint64          `json:"generation"`
	IssuedAt      time.Time       `json:"issued_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	ProxyCore     string          `json:"proxy_core"`
	Transport     ClientTransport `json:"transport"`
	WireGuard     ClientWireGuard `json:"wireguard"`
	Signature     Signature       `json:"signature"`
}

func DecodeSignedConfig(raw []byte) (SignedConfig, error) {
	if err := rejectForbiddenSecretFields(raw); err != nil {
		return SignedConfig{}, err
	}
	config, err := strictDecode[SignedConfig](raw)
	if err != nil {
		return SignedConfig{}, fmt.Errorf("decode signed config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return SignedConfig{}, err
	}
	return config, nil
}

func (c SignedConfig) Validate() error {
	if c.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("schema_version must be %d", SchemaVersionV1)
	}
	for label, value := range map[string]string{
		"config_id":  c.ConfigID,
		"network_id": c.NetworkID,
		"device_id":  c.DeviceID,
	} {
		if err := validateID(label, value); err != nil {
			return err
		}
	}
	if c.Generation == 0 {
		return errors.New("generation must be greater than zero")
	}
	if c.IssuedAt.IsZero() || !c.ExpiresAt.After(c.IssuedAt) {
		return errors.New("expires_at must be after issued_at")
	}
	if c.ProxyCore != ProxyCoreXray {
		return fmt.Errorf("proxy_core must be %q", ProxyCoreXray)
	}
	if err := c.Transport.Validate(); err != nil {
		return err
	}
	if !interfacePattern.MatchString(c.WireGuard.InterfaceName) {
		return errors.New("wireguard interface_name is invalid")
	}
	if err := validateCIDRs("wireguard addresses", c.WireGuard.Addresses); err != nil {
		return err
	}
	if c.WireGuard.MTU < 576 || c.WireGuard.MTU > 1500 {
		return errors.New("wireguard mtu must be between 576 and 1500")
	}
	if len(c.WireGuard.Peers) == 0 {
		return errors.New("at least one wireguard peer is required")
	}
	seenGateways := make(map[string]struct{}, len(c.WireGuard.Peers))
	for index, peer := range c.WireGuard.Peers {
		if err := validateID("wireguard peer gateway_id", peer.GatewayID); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if _, exists := seenGateways[peer.GatewayID]; exists {
			return fmt.Errorf("wireguard peer gateway_id %q is duplicated", peer.GatewayID)
		}
		seenGateways[peer.GatewayID] = struct{}{}
		if err := validateWireGuardKey(peer.PublicKey); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if err := validateCIDRs("wireguard peer allowed_ips", peer.AllowedIPs); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if peer.Endpoint.Host != LoopbackHost {
			return fmt.Errorf("wireguard peer %d endpoint host must be %q", index, LoopbackHost)
		}
		if err := validatePort("wireguard peer endpoint port", peer.Endpoint.Port); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if peer.PersistentKeepaliveSeconds < 0 || peer.PersistentKeepaliveSeconds > 65535 {
			return fmt.Errorf("wireguard peer %d persistent_keepalive_seconds is invalid", index)
		}
	}
	return c.Signature.Validate()
}

type GatewaySafety struct {
	AllowEmptyPeers       bool    `json:"allow_empty_peers"`
	MaxPeerRemovalPercent float64 `json:"max_peer_removal_percent"`
}

type GatewayPeer struct {
	DeviceID                   string   `json:"device_id"`
	PublicKey                  string   `json:"public_key"`
	AllowedIPs                 []string `json:"allowed_ips"`
	PersistentKeepaliveSeconds int      `json:"persistent_keepalive_seconds,omitempty"`
}

type GatewayWireGuard struct {
	InterfaceName string        `json:"interface_name"`
	ListenPort    int           `json:"listen_port"`
	Addresses     []string      `json:"addresses"`
	Peers         []GatewayPeer `json:"peers"`
}

type GatewayRelay struct {
	Transport      string   `json:"transport"`
	ListenHost     string   `json:"listen_host"`
	ListenPort     int      `json:"listen_port"`
	ServerNames    []string `json:"server_names"`
	CredentialRefs []string `json:"credential_refs"`
}

type GatewayPolicy struct {
	Generation    uint64 `json:"generation"`
	Backend       string `json:"backend"`
	RulesetSHA256 string `json:"ruleset_sha256"`
}

type GatewaySnapshot struct {
	SchemaVersion              int              `json:"schema_version"`
	SnapshotID                 string           `json:"snapshot_id"`
	NodeID                     string           `json:"node_id"`
	Generation                 uint64           `json:"generation"`
	ExpectedPreviousGeneration uint64           `json:"expected_previous_generation"`
	IssuedAt                   time.Time        `json:"issued_at"`
	ExpiresAt                  time.Time        `json:"expires_at"`
	ProxyCore                  string           `json:"proxy_core"`
	Safety                     GatewaySafety    `json:"safety"`
	WireGuard                  GatewayWireGuard `json:"wireguard"`
	Relay                      GatewayRelay     `json:"relay"`
	Policy                     GatewayPolicy    `json:"policy"`
	Signature                  Signature        `json:"signature"`
}

func DecodeGatewaySnapshot(raw []byte) (GatewaySnapshot, error) {
	if err := rejectForbiddenSecretFields(raw); err != nil {
		return GatewaySnapshot{}, err
	}
	snapshot, err := strictDecode[GatewaySnapshot](raw)
	if err != nil {
		return GatewaySnapshot{}, fmt.Errorf("decode gateway snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return GatewaySnapshot{}, err
	}
	return snapshot, nil
}

func (s GatewaySnapshot) Validate() error {
	if s.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("schema_version must be %d", SchemaVersionV1)
	}
	if err := validateID("snapshot_id", s.SnapshotID); err != nil {
		return err
	}
	if err := validateID("node_id", s.NodeID); err != nil {
		return err
	}
	if s.Generation == 0 || s.Generation <= s.ExpectedPreviousGeneration {
		return errors.New("generation must advance expected_previous_generation")
	}
	if s.IssuedAt.IsZero() || !s.ExpiresAt.After(s.IssuedAt) {
		return errors.New("expires_at must be after issued_at")
	}
	if s.ProxyCore != ProxyCoreXray {
		return fmt.Errorf("proxy_core must be %q", ProxyCoreXray)
	}
	if s.Safety.MaxPeerRemovalPercent < 0 || s.Safety.MaxPeerRemovalPercent > 100 {
		return errors.New("safety max_peer_removal_percent must be between 0 and 100")
	}
	if len(s.WireGuard.Peers) == 0 && !s.Safety.AllowEmptyPeers {
		return errors.New("empty peers require an explicit safety override")
	}
	if !interfacePattern.MatchString(s.WireGuard.InterfaceName) {
		return errors.New("wireguard interface_name is invalid")
	}
	if err := validatePort("wireguard listen_port", s.WireGuard.ListenPort); err != nil {
		return err
	}
	if err := validateCIDRs("wireguard addresses", s.WireGuard.Addresses); err != nil {
		return err
	}
	seenDevices := make(map[string]struct{}, len(s.WireGuard.Peers))
	for index, peer := range s.WireGuard.Peers {
		if err := validateID("wireguard peer device_id", peer.DeviceID); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if _, exists := seenDevices[peer.DeviceID]; exists {
			return fmt.Errorf("wireguard peer device_id %q is duplicated", peer.DeviceID)
		}
		seenDevices[peer.DeviceID] = struct{}{}
		if err := validateWireGuardKey(peer.PublicKey); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if err := validateCIDRs("wireguard peer allowed_ips", peer.AllowedIPs); err != nil {
			return fmt.Errorf("wireguard peer %d: %w", index, err)
		}
		if peer.PersistentKeepaliveSeconds < 0 || peer.PersistentKeepaliveSeconds > 65535 {
			return fmt.Errorf("wireguard peer %d persistent_keepalive_seconds is invalid", index)
		}
	}
	if s.Relay.Transport != TransportVLESS {
		return fmt.Errorf("relay transport must be %q", TransportVLESS)
	}
	if err := validateHost("relay listen_host", s.Relay.ListenHost); err != nil {
		return err
	}
	if err := validatePort("relay listen_port", s.Relay.ListenPort); err != nil {
		return err
	}
	if len(s.Relay.ServerNames) == 0 || len(s.Relay.CredentialRefs) == 0 {
		return errors.New("relay server_names and credential_refs are required")
	}
	if err := validateUniqueStrings("relay server_names", s.Relay.ServerNames); err != nil {
		return err
	}
	if err := validateUniqueStrings("relay credential_refs", s.Relay.CredentialRefs); err != nil {
		return err
	}
	for _, serverName := range s.Relay.ServerNames {
		if err := validateHost("relay server_name", serverName); err != nil {
			return err
		}
	}
	for _, credentialRef := range s.Relay.CredentialRefs {
		if err := validateID("relay credential_ref", credentialRef); err != nil {
			return err
		}
	}
	if s.Policy.Generation == 0 || s.Policy.Backend != PolicyNFTables {
		return errors.New("policy requires a positive generation and nftables backend")
	}
	digest, err := hex.DecodeString(s.Policy.RulesetSHA256)
	if err != nil || len(digest) != 32 || s.Policy.RulesetSHA256 != strings.ToLower(s.Policy.RulesetSHA256) {
		return errors.New("policy ruleset_sha256 must be 64 lowercase hexadecimal characters")
	}
	return s.Signature.Validate()
}

// ValidateTransition checks safety constraints that require the last applied
// snapshot. Callers must run this before replacing a gateway's last-known-good
// state.
func (s GatewaySnapshot) ValidateTransition(previous GatewaySnapshot) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.ExpectedPreviousGeneration != previous.Generation {
		return fmt.Errorf("expected_previous_generation %d does not match applied generation %d", s.ExpectedPreviousGeneration, previous.Generation)
	}
	if s.Generation <= previous.Generation {
		return errors.New("generation must advance the applied generation")
	}
	if len(previous.WireGuard.Peers) == 0 {
		return nil
	}
	currentDevices := make(map[string]struct{}, len(s.WireGuard.Peers))
	for _, peer := range s.WireGuard.Peers {
		currentDevices[peer.DeviceID] = struct{}{}
	}
	removed := 0
	for _, peer := range previous.WireGuard.Peers {
		if _, exists := currentDevices[peer.DeviceID]; !exists {
			removed++
		}
	}
	removalPercent := float64(removed) * 100 / float64(len(previous.WireGuard.Peers))
	if removalPercent > s.Safety.MaxPeerRemovalPercent {
		return fmt.Errorf("peer removal %.2f%% exceeds safety limit %.2f%%", removalPercent, s.Safety.MaxPeerRemovalPercent)
	}
	return nil
}

func strictDecode[T any](raw []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, errors.New("multiple JSON values are not allowed")
		}
		return value, err
	}
	return value, nil
}

func rejectForbiddenSecretFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode contract for secret-field validation: %w", err)
	}
	return walkForbiddenFields(document)
}

func walkForbiddenFields(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, forbidden := forbiddenFields[strings.ToLower(key)]; forbidden {
				return fmt.Errorf("forbidden secret field: %s", key)
			}
			if err := walkForbiddenFields(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := walkForbiddenFields(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateID(label, value string) error {
	if !idPattern.MatchString(value) {
		return fmt.Errorf("%s is invalid", label)
	}
	return nil
}

func validatePort(label string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535", label)
	}
	return nil
}

func validateHost(label, host string) error {
	if strings.TrimSpace(host) == "" || len(host) > 253 {
		return fmt.Errorf("%s is invalid", label)
	}
	return nil
}

func validateWireGuardKey(value string) error {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return errors.New("wireguard public_key must be a base64-encoded 32-byte key")
	}
	return nil
}

func validateCIDRs(label string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s must not be empty", label)
	}
	if err := validateUniqueStrings(label, values); err != nil {
		return err
	}
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() {
			return fmt.Errorf("%s contains invalid IPv4 CIDR %q", label, value)
		}
	}
	return nil
}

func validateUniqueStrings(label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate value %q", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}
