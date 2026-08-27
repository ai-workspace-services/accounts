package domain

import (
	"strings"
	"testing"
	"time"
)

const (
	validWGKey     = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	validSignature = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
)

func validSignedConfig() SignedConfig {
	issuedAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return SignedConfig{
		SchemaVersion: SchemaVersionV1,
		ConfigID:      "cfg_01xconnect",
		NetworkID:     "net_private",
		DeviceID:      "dev_laptop",
		Generation:    42,
		IssuedAt:      issuedAt,
		ExpiresAt:     issuedAt.Add(24 * time.Hour),
		ProxyCore:     ProxyCoreXray,
		Transport: ClientTransport{
			Kind:     TransportVLESS,
			Loopback: Endpoint{Host: LoopbackHost, Port: 51830},
			Remote:   RemoteEndpoint{Host: "gateway.example.net", Port: 443, ServerName: "gateway.example.net"},
			AuthID:   "auth_device_01",
		},
		WireGuard: ClientWireGuard{
			InterfaceName: "wg-xco",
			Addresses:     []string{"10.77.0.10/32"},
			MTU:           1280,
			Peers: []ClientPeer{{
				GatewayID: "gw_tokyo_01", PublicKey: validWGKey, AllowedIPs: []string{"10.77.0.0/16"},
				Endpoint: Endpoint{Host: LoopbackHost, Port: 51830}, PersistentKeepaliveSeconds: 25,
			}},
		},
		Signature: Signature{Algorithm: SignatureEd25519, KeyID: "signing_key_01", Value: validSignature},
	}
}

func validGatewaySnapshot() GatewaySnapshot {
	issuedAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return GatewaySnapshot{
		SchemaVersion:              SchemaVersionV1,
		SnapshotID:                 "snap_tokyo_42",
		NodeID:                     "gw_tokyo_01",
		Generation:                 42,
		ExpectedPreviousGeneration: 41,
		IssuedAt:                   issuedAt,
		ExpiresAt:                  issuedAt.Add(24 * time.Hour),
		ProxyCore:                  ProxyCoreXray,
		Safety:                     GatewaySafety{AllowEmptyPeers: false, MaxPeerRemovalPercent: 20},
		WireGuard: GatewayWireGuard{
			InterfaceName: "wg-xco", ListenPort: 51820, Addresses: []string{"10.77.0.1/32"},
			Peers: []GatewayPeer{{DeviceID: "dev_laptop", PublicKey: validWGKey, AllowedIPs: []string{"10.77.0.10/32"}}},
		},
		Relay: GatewayRelay{
			Transport: TransportVLESS, ListenHost: "0.0.0.0", ListenPort: 443,
			ServerNames: []string{"gateway.example.net"}, CredentialRefs: []string{"vault_xconnect_relay_01"},
		},
		Policy: GatewayPolicy{
			Generation: 7, Backend: PolicyNFTables,
			RulesetSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		Signature: Signature{Algorithm: SignatureEd25519, KeyID: "signing_key_01", Value: validSignature},
	}
}

func TestSignedConfigValidate(t *testing.T) {
	if err := validSignedConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestSignedConfigRejectsUnsafeNetworkAndCryptoValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SignedConfig)
		want   string
	}{
		{name: "unsupported proxy core", mutate: func(config *SignedConfig) { config.ProxyCore = "other-core" }, want: "proxy_core"},
		{name: "invalid address", mutate: func(config *SignedConfig) { config.WireGuard.Addresses[0] = "999.77.0.10/32" }, want: "invalid IPv4 CIDR"},
		{name: "invalid public key", mutate: func(config *SignedConfig) { config.WireGuard.Peers[0].PublicKey = "not-a-key" }, want: "32-byte key"},
		{name: "invalid signature", mutate: func(config *SignedConfig) { config.Signature.Value = "not-a-signature" }, want: "64-byte Ed25519"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validSignedConfig()
			test.mutate(&config)
			if err := config.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestGatewaySnapshotGenerationAndEmptyPeerSafety(t *testing.T) {
	snapshot := validGatewaySnapshot()
	snapshot.Generation = snapshot.ExpectedPreviousGeneration
	if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), "generation must advance") {
		t.Fatalf("expected generation safety error, got %v", err)
	}

	snapshot = validGatewaySnapshot()
	snapshot.WireGuard.Peers = nil
	if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), "explicit safety override") {
		t.Fatalf("expected empty peer safety error, got %v", err)
	}
	snapshot.Safety.AllowEmptyPeers = true
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("explicitly authorized empty peer set rejected: %v", err)
	}
}

func TestGatewaySnapshotTransitionEnforcesPeerRemovalLimit(t *testing.T) {
	previous := validGatewaySnapshot()
	previous.Generation = 41
	previous.ExpectedPreviousGeneration = 40
	previous.WireGuard.Peers = append(previous.WireGuard.Peers, GatewayPeer{
		DeviceID: "dev_phone", PublicKey: validWGKey, AllowedIPs: []string{"10.77.0.11/32"},
	})

	next := validGatewaySnapshot()
	next.Safety.MaxPeerRemovalPercent = 20
	if err := next.ValidateTransition(previous); err == nil || !strings.Contains(err.Error(), "exceeds safety limit") {
		t.Fatalf("expected peer removal safety error, got %v", err)
	}
	next.Safety.MaxPeerRemovalPercent = 50
	if err := next.ValidateTransition(previous); err != nil {
		t.Fatalf("allowed peer removal rejected: %v", err)
	}
}

func TestGatewaySnapshotRejectsUnsafeNetworkAndCryptoValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GatewaySnapshot)
		want   string
	}{
		{name: "invalid allowed IP", mutate: func(snapshot *GatewaySnapshot) { snapshot.WireGuard.Peers[0].AllowedIPs[0] = "999.77.0.10/32" }, want: "invalid IPv4 CIDR"},
		{name: "invalid public key", mutate: func(snapshot *GatewaySnapshot) { snapshot.WireGuard.Peers[0].PublicKey = "not-a-key" }, want: "32-byte key"},
		{name: "invalid signature", mutate: func(snapshot *GatewaySnapshot) { snapshot.Signature.Value = "not-a-signature" }, want: "64-byte Ed25519"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validGatewaySnapshot()
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestStrictDecodeRejectsForbiddenSecretFields(t *testing.T) {
	tests := []struct {
		field string
		raw   string
	}{
		{field: "private_key", raw: `{"wireguard":{"private_key":"must-not-cross-control-plane"}}`},
		{field: "refresh_token", raw: `{"refresh_token":"must-not-cross-control-plane"}`},
		{field: "vault_token", raw: `{"relay":{"auth":{"vault_token":"must-not-cross-control-plane"}}}`},
	}
	for _, test := range tests {
		if _, err := DecodeSignedConfig([]byte(test.raw)); err == nil || !strings.Contains(err.Error(), "forbidden secret field: "+test.field) {
			t.Fatalf("expected %s rejection, got %v", test.field, err)
		}
	}
}
