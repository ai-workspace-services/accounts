package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validSignedConfig() SignedConfig {
	issuedAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return SignedConfig{
		SchemaVersion: SchemaVersionV1,
		Generation:    42,
		IssuedAt:      issuedAt,
		ExpiresAt:     issuedAt.Add(24 * time.Hour),
		Network:       NetworkConfig{ID: "xworkmate-private", Address: "172.29.10.100/32"},
		WireGuard: WireGuardConfig{Peers: []WireGuardPeer{{
			NodeID: "xworkmate-bridge", PublicKey: "public-key", AllowedIPs: []string{"172.29.10.0/24"}, Endpoint: "127.0.0.1:51830",
		}}},
		Transport: TransportConfig{ProxyCore: ProxyCoreXray, Type: TransportVLESS, Host: "xworkmate-bridge.svc.plus", Port: 2443, CredentialRef: "sealed:fixture"},
		Policy:    PolicyConfig{Revision: 7, ArtifactHash: "sha256:fixture", Rules: []json.RawMessage{}},
		Signature: Signature{KeyID: "overlay-config-2026-01", Algorithm: SignatureEd25519, Value: "fixture-signature"},
	}
}

func TestSignedConfigValidate(t *testing.T) {
	if err := validSignedConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestSignedConfigRejectsUnsupportedProxyCore(t *testing.T) {
	config := validSignedConfig()
	config.Transport.ProxyCore = "sing-box"
	err := config.Validate()
	if err == nil || !strings.Contains(err.Error(), "proxy_core") {
		t.Fatalf("expected unsupported proxy core error, got %v", err)
	}
}

func TestGatewaySnapshotRejectsInvalidRuntimeConfig(t *testing.T) {
	issuedAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	snapshot := GatewaySnapshot{
		SchemaVersion: SchemaVersionV1,
		NodeID:        "xworkmate-bridge",
		Generation:    42,
		IssuedAt:      issuedAt,
		ExpiresAt:     issuedAt.Add(time.Hour),
		Interface:     GatewayInterface{Name: "wg-xwm", Address: "172.29.10.1/24", ListenPort: 51820},
		Policy:        PolicyConfig{Revision: 7, ArtifactHash: "sha256:fixture"},
		Transport:     GatewayTransport{ProxyCore: ProxyCoreXray, RuntimeConfig: json.RawMessage(`not-json`)},
		Signature:     Signature{KeyID: "overlay-config-2026-01", Algorithm: SignatureEd25519, Value: "fixture-signature"},
	}
	if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), "runtime_config") {
		t.Fatalf("expected invalid runtime config error, got %v", err)
	}
}
