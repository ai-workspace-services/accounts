package overlay

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fullMeshPolicy(network string, generation uint64, ids []string) PolicyArtifact {
	p := defaultPolicyArtifact(network, generation)
	ports := make([]int, 65535)
	for i := range ports {
		ports[i] = i + 1
	}
	p.Rules = []PolicyRule{{WholeDevice: true, ID: "all-traffic", Action: "accept", SourceDevices: ids, DestinationDevices: ids, Protocols: []string{"icmp", "tcp", "udp"}, Ports: ports}}
	return p
}
func TestWholeDeviceEligibilityNeverWidensRestrictedPolicy(t *testing.T) {
	p := fullMeshPolicy("net", 1, []string{"one-a", "one-b"})
	if !wholeDeviceAllowed(p, "one-a", "one-b") {
		t.Fatal("full grant rejected")
	}
	p.Rules[0].WholeDevice = false
	if wholeDeviceAllowed(p, "one-a", "one-b") {
		t.Fatal("protocol-scoped rule silently became whole-device grant")
	}
	p.Rules[0].WholeDevice = true
	p.Rules[0].Ports = []int{443}
	if wholeDeviceAllowed(p, "one-a", "one-b") {
		t.Fatal("port-limited grant widened")
	}
	p = fullMeshPolicy("net", 1, []string{"one-a", "one-b"})
	p.Rules = append(p.Rules, PolicyRule{ID: "deny-ssh", Action: "deny", SourceDevices: []string{"one-a"}, DestinationDevices: []string{"one-b"}, Protocols: []string{"tcp"}, Ports: []int{22}})
	if wholeDeviceAllowed(p, "one-a", "one-b") {
		t.Fatal("deny ignored")
	}
	if wholeDeviceAllowed(defaultPolicyArtifact("net", 1), "one-a", "one-b") {
		t.Fatal("default deny ignored")
	}
}

// When requested by the integration runner, export ephemeral fixtures. Files
// include test-only private keys and are confined to its protected temp folder.
func TestMeshMultiDeviceSignedFixtures(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "mesh.db")), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(db, Config{MeshNetworks: []string{"mesh-net"}})
	if e != nil {
		t.Fatal(e)
	}
	private := map[string]string{}
	public := map[string]string{}
	for _, id := range []string{"gw-mesh", "one-a", "one-b", "one-c", "one-mobile"} {
		key, e := ecdh.X25519().GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		private[id] = base64.StdEncoding.EncodeToString(key.Bytes())
		public[id] = base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
	}
	network := NetworkRecord{ID: "mesh-net", OwnerUserID: "mesh-owner", CIDR: "10.79.0.0/24", GatewayID: "gw-mesh", GatewayWireGuardKey: public["gw-mesh"], GatewayWireGuardAddress: "10.79.0.1/32", GatewayEndpointHost: "gateway.example.test", GatewayEndpointPort: 51820, TransportServerName: "gateway.example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", ConfigGeneration: 1}
	policy := fullMeshPolicy(network.ID, 1, []string{"one-a", "one-b", "one-c", "one-mobile"})
	raw, _ := json.Marshal(policy)
	network.PolicyJSON = string(raw)
	if e = db.Create(&network).Error; e != nil {
		t.Fatal(e)
	}
	devices := []DeviceRecord{{ID: "one-a", Platform: "linux", WireGuardAddress: "10.79.0.2/32"}, {ID: "one-b", Platform: "darwin", WireGuardAddress: "10.79.0.3/32"}, {ID: "one-c", Platform: "windows", WireGuardAddress: "10.79.0.4/32"}, {ID: "one-mobile", Platform: "ios", WireGuardAddress: "10.79.0.5/32"}}
	for i := range devices {
		devices[i].Role = RoleOne
		devices[i].Status = "active"
		devices[i].NetworkID = network.ID
		devices[i].WireGuardPublicKey = public[devices[i].ID]
		if e = db.Create(&devices[i]).Error; e != nil {
			t.Fatal(e)
		}
	}
	relay, e := svc.relaySpec(context.Background(), network)
	if e != nil || len(relay.Peers) != 3 {
		t.Fatalf("relay peer selection: %v", e)
	}
	fixtures := struct {
		One     []SignedConfig      `json:"one"`
		Gateway GatewaySignedConfig `json:"gateway"`
		Keys    []SigningKey        `json:"keys"`
		Policy  json.RawMessage     `json:"policy"`
		Private map[string]string   `json:"private"`
	}{Private: private, Policy: json.RawMessage(network.PolicyJSON)}
	for _, d := range devices {
		cfg, _, e := svc.buildSignedConfig(d, network, true)
		if e != nil {
			t.Fatal(e)
		}
		if d.Platform == "ios" {
			if cfg.Mesh != nil {
				t.Fatal("unsupported device advertised mesh")
			}
			continue
		}
		if cfg.Mesh == nil || len(cfg.Mesh.Peers) != 2 {
			t.Fatal("multi-peer config missing")
		}
		fixtures.One = append(fixtures.One, cfg)
	}
	peers := make([]GatewayPeer, 0, 3)
	for _, d := range devices[:3] {
		peers = append(peers, GatewayPeer{DeviceID: d.ID, WireGuardPublicKey: d.WireGuardPublicKey, WireGuardAddress: d.WireGuardAddress, AllowedIPs: d.WireGuardAddress})
	}
	now := svc.now()
	g := GatewaySignedConfig{SchemaVersion: 1, Role: RoleGateway, ConfigID: "mesh-gateway-config", NetworkID: network.ID, GatewayID: network.GatewayID, Generation: 1, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), InterfaceName: "xconzero0", Address: network.GatewayWireGuardAddress, ListenPort: 51820, MTU: 1280, Peers: peers, Transport: GatewayTransport{Kind: TransportVLESSXHTTP, ServerName: network.TransportServerName, Port: 443, AuthID: network.TransportAuthID, Path: DefaultTransportPath, Mode: "auto", Host: network.TransportServerName, Frontend: GatewayFrontendDirectTLS}, Mesh: relay}
	payload, _ := gatewaySigningBytes(g)
	g.Signature = Signature{Algorithm: "Ed25519", KeyID: svc.keyID, Value: base64.StdEncoding.EncodeToString(ed25519.Sign(svc.privateKey, payload))}
	fixtures.Gateway = g
	fixtures.Keys = svc.SigningKeys(now)
	if dir := os.Getenv("XCONNECT_MESH_FIXTURE_DIR"); dir != "" {
		if e = os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(fixtures)
		if e = os.WriteFile(filepath.Join(dir, "fixtures.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	// Revocation changes both One peer lists and relay grants.
	if e = svc.AdminRevokeDevice(context.Background(), "mesh-owner", "one-b"); e != nil {
		t.Fatal(e)
	}
	if e = db.Where("id = ?", network.ID).First(&network).Error; e != nil {
		t.Fatal(e)
	}
	var updatedPolicy PolicyArtifact
	if e = json.Unmarshal([]byte(network.PolicyJSON), &updatedPolicy); e != nil {
		t.Fatal(e)
	}
	if network.ConfigGeneration != 2 || updatedPolicy.Revision != network.ConfigGeneration {
		t.Fatal("revocation generation and policy diverged")
	}
	mesh, e := svc.oneMeshSpec(context.Background(), devices[0], network)
	if e != nil || len(mesh.Peers) != 1 || mesh.Peers[0].DeviceID != "one-c" {
		t.Fatal("revoked peer retained")
	}
}
