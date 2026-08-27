// Package domain contains transport-neutral XConnect-One overlay contracts.
// It deliberately contains no HTTP, persistence, or platform runtime code so
// the controller, generated clients, and gateway agent can share the same
// compatibility rules.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	SchemaVersionV1  = 1
	ProxyCoreXray    = "xray"
	TransportVLESS   = "vless-tls-xudp"
	SignatureEd25519 = "Ed25519"
)

type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

func (s Signature) Validate() error {
	if strings.TrimSpace(s.KeyID) == "" {
		return errors.New("signature key_id is required")
	}
	if s.Algorithm != SignatureEd25519 {
		return fmt.Errorf("signature algorithm must be %q", SignatureEd25519)
	}
	if strings.TrimSpace(s.Value) == "" {
		return errors.New("signature value is required")
	}
	return nil
}

type NetworkConfig struct {
	ID      string   `json:"id"`
	Address string   `json:"address"`
	DNS     []string `json:"dns"`
}

type WireGuardPeer struct {
	NodeID     string   `json:"node_id"`
	PublicKey  string   `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
	Endpoint   string   `json:"endpoint"`
}

type WireGuardConfig struct {
	Peers []WireGuardPeer `json:"peers"`
}

type TransportConfig struct {
	ProxyCore     string `json:"proxy_core"`
	Type          string `json:"type"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	CredentialRef string `json:"credential_ref"`
}

func (t TransportConfig) Validate() error {
	if t.ProxyCore != ProxyCoreXray {
		return fmt.Errorf("proxy_core must be %q", ProxyCoreXray)
	}
	if t.Type != TransportVLESS {
		return fmt.Errorf("transport type must be %q", TransportVLESS)
	}
	if strings.TrimSpace(t.Host) == "" {
		return errors.New("transport host is required")
	}
	if t.Port < 1 || t.Port > 65535 {
		return errors.New("transport port must be between 1 and 65535")
	}
	if strings.TrimSpace(t.CredentialRef) == "" {
		return errors.New("transport credential_ref is required")
	}
	return nil
}

type PolicyConfig struct {
	Revision     uint64            `json:"revision"`
	ArtifactHash string            `json:"artifact_hash"`
	Rules        []json.RawMessage `json:"rules"`
}

type SignedConfig struct {
	SchemaVersion    int             `json:"schema_version"`
	Generation       uint64          `json:"generation"`
	IssuedAt         time.Time       `json:"issued_at"`
	ExpiresAt        time.Time       `json:"expires_at"`
	MinClientVersion string          `json:"min_client_version,omitempty"`
	Network          NetworkConfig   `json:"network"`
	WireGuard        WireGuardConfig `json:"wireguard"`
	Transport        TransportConfig `json:"transport"`
	Policy           PolicyConfig    `json:"policy"`
	Signature        Signature       `json:"signature"`
}

func (c SignedConfig) Validate() error {
	if c.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("schema_version must be %d", SchemaVersionV1)
	}
	if c.Generation == 0 {
		return errors.New("generation must be greater than zero")
	}
	if c.IssuedAt.IsZero() || !c.ExpiresAt.After(c.IssuedAt) {
		return errors.New("expires_at must be after issued_at")
	}
	if strings.TrimSpace(c.Network.ID) == "" || strings.TrimSpace(c.Network.Address) == "" {
		return errors.New("network id and address are required")
	}
	if len(c.WireGuard.Peers) == 0 {
		return errors.New("at least one wireguard peer is required")
	}
	for index, peer := range c.WireGuard.Peers {
		if strings.TrimSpace(peer.NodeID) == "" || strings.TrimSpace(peer.PublicKey) == "" || strings.TrimSpace(peer.Endpoint) == "" || len(peer.AllowedIPs) == 0 {
			return fmt.Errorf("wireguard peer %d is incomplete", index)
		}
	}
	if err := c.Transport.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Policy.ArtifactHash) == "" {
		return errors.New("policy artifact_hash is required")
	}
	return c.Signature.Validate()
}

type GatewayPeer struct {
	DeviceID   string   `json:"device_id"`
	PublicKey  string   `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
}

type GatewayInterface struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	ListenPort int    `json:"listen_port"`
}

type GatewayTransport struct {
	ProxyCore     string          `json:"proxy_core"`
	RuntimeConfig json.RawMessage `json:"runtime_config"`
}

type GatewaySnapshot struct {
	SchemaVersion     int              `json:"schema_version"`
	NodeID            string           `json:"node_id"`
	Generation        uint64           `json:"generation"`
	IssuedAt          time.Time        `json:"issued_at"`
	ExpiresAt         time.Time        `json:"expires_at"`
	MinGatewayVersion string           `json:"min_gateway_version,omitempty"`
	Interface         GatewayInterface `json:"interface"`
	Peers             []GatewayPeer    `json:"peers"`
	Policy            PolicyConfig     `json:"policy"`
	Transport         GatewayTransport `json:"transport"`
	Signature         Signature        `json:"signature"`
}

func (s GatewaySnapshot) Validate() error {
	if s.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("schema_version must be %d", SchemaVersionV1)
	}
	if strings.TrimSpace(s.NodeID) == "" || s.Generation == 0 {
		return errors.New("node_id and positive generation are required")
	}
	if s.IssuedAt.IsZero() || !s.ExpiresAt.After(s.IssuedAt) {
		return errors.New("expires_at must be after issued_at")
	}
	if strings.TrimSpace(s.Interface.Name) == "" || strings.TrimSpace(s.Interface.Address) == "" {
		return errors.New("gateway interface name and address are required")
	}
	if s.Interface.ListenPort < 1 || s.Interface.ListenPort > 65535 {
		return errors.New("gateway interface listen_port must be between 1 and 65535")
	}
	for index, peer := range s.Peers {
		if strings.TrimSpace(peer.DeviceID) == "" || strings.TrimSpace(peer.PublicKey) == "" || len(peer.AllowedIPs) == 0 {
			return fmt.Errorf("gateway peer %d is incomplete", index)
		}
	}
	if strings.TrimSpace(s.Policy.ArtifactHash) == "" {
		return errors.New("policy artifact_hash is required")
	}
	if s.Transport.ProxyCore != ProxyCoreXray {
		return fmt.Errorf("proxy_core must be %q", ProxyCoreXray)
	}
	if len(s.Transport.RuntimeConfig) == 0 || !json.Valid(s.Transport.RuntimeConfig) {
		return errors.New("transport runtime_config must be valid JSON")
	}
	return s.Signature.Validate()
}
