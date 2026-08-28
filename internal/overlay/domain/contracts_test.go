package domain

import (
	"crypto/ed25519"
	"encoding/base64"
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

func TestGatewaySnapshotSigningGoldenVectorMatchesAgent(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index)
	}
	snapshot := validGatewaySnapshot()
	snapshot.SnapshotID = "snap_vector_42"
	snapshot.NodeID = "gw_test_01"
	snapshot.Generation = 42
	snapshot.ExpectedPreviousGeneration = 41
	snapshot.IssuedAt = time.Date(2026, 8, 28, 11, 59, 0, 0, time.UTC)
	snapshot.ExpiresAt = time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	snapshot.Safety.MaxPeerRemovalPercent = 100
	snapshot.WireGuard.Peers = []GatewayPeer{{DeviceID: "dev_test_01", PublicKey: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=", AllowedIPs: []string{"10.77.0.10/32"}, PersistentKeepaliveSeconds: 25}}
	snapshot.Relay = GatewayRelay{Transport: "vless-tls-xudp", ListenHost: "0.0.0.0", ListenPort: 443, ServerNames: []string{"gateway.example"}, CredentialRefs: []string{"vault_test_01"}}
	snapshot.Policy = GatewayPolicy{Generation: 1, Backend: "nftables", RulesetSHA256: strings.Repeat("b", 64)}
	payload, err := snapshot.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"snapshot_id":"snap_vector_42","node_id":"gw_test_01","generation":42,"expected_previous_generation":41,"issued_at":"2026-08-28T11:59:00Z","expires_at":"2026-08-28T13:00:00Z","proxy_core":"xray","safety":{"allow_empty_peers":false,"max_peer_removal_percent":100},"wireguard":{"interface_name":"wg-xco","listen_port":51820,"addresses":["10.77.0.1/32"],"peers":[{"device_id":"dev_test_01","public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=","allowed_ips":["10.77.0.10/32"],"persistent_keepalive_seconds":25}]},"relay":{"transport":"vless-tls-xudp","listen_host":"0.0.0.0","listen_port":443,"server_names":["gateway.example"],"credential_refs":["vault_test_01"]},"policy":{"generation":1,"backend":"nftables","ruleset_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`
	if string(payload) != want {
		t.Fatalf("gateway signing bytes drifted\ngot:  %s\nwant: %s", payload, want)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), payload))
	if signature != "6ntypTf83jAGH4aTxIsmkPvGnBiI+3d+YLmAtRLi2G6d/BZW/PPB00ANbMH/yVrg+cOOpDDQMSDtKB8WUeIyBw==" {
		t.Fatalf("gateway signature vector drifted: %s", signature)
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
