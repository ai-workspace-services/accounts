package projection

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"account/internal/overlay/domain"
)

func TestSignedConfigEd25519GoldenVector(t *testing.T) {
	var vector struct {
		SeedBase64         string `json:"seed_base64"`
		PublicKeyBase64    string `json:"public_key_base64"`
		KeyID              string `json:"key_id"`
		SigningPayloadUTF8 string `json:"signing_payload_utf8"`
		SignatureBase64    string `json:"signature_base64"`
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve signing vector path")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "..", "tests", "fixtures", "overlay", "signed-config-ed25519-vector.json"))
	if err != nil {
		t.Fatalf("read signing vector: %v", err)
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("decode signing vector: %v", err)
	}
	config := domain.SignedConfig{
		SchemaVersion: 1,
		ConfigID:      "cfg_01xconnect",
		NetworkID:     "net_private",
		DeviceID:      "dev_laptop",
		Generation:    42,
		IssuedAt:      time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		ExpiresAt:     time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		ProxyCore:     domain.ProxyCoreXray,
		Transport: domain.ClientTransport{
			Kind:     domain.TransportVLESS,
			Loopback: domain.Endpoint{Host: domain.LoopbackHost, Port: 51830},
			Remote:   domain.RemoteEndpoint{Host: "gateway.example.net", Port: 443, ServerName: "gateway.example.net"},
			AuthID:   "auth_device_01",
		},
		WireGuard: domain.ClientWireGuard{
			InterfaceName: "wg-xco",
			Addresses:     []string{"10.77.0.10/32"},
			MTU:           1280,
			Peers: []domain.ClientPeer{{
				GatewayID:                  "gw_tokyo_01",
				PublicKey:                  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				AllowedIPs:                 []string{"10.77.0.0/16"},
				Endpoint:                   domain.Endpoint{Host: domain.LoopbackHost, Port: 51830},
				PersistentKeepaliveSeconds: 25,
			}},
		},
	}
	payload, err := config.SigningBytes()
	if err != nil {
		t.Fatalf("encode signing payload: %v", err)
	}
	if string(payload) != vector.SigningPayloadUTF8 {
		t.Fatalf("signing payload drifted\ngot:  %s\nwant: %s", payload, vector.SigningPayloadUTF8)
	}
	seed, err := base64.StdEncoding.DecodeString(vector.SeedBase64)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("invalid vector seed: length=%d err=%v", len(seed), err)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	signer, err := NewEd25519Signer(privateKey, vector.KeyID)
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}
	signature, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign payload: %v", err)
	}
	gotPublicKey := base64.StdEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	if gotPublicKey != vector.PublicKeyBase64 {
		t.Errorf("public key vector = %s, want %s", gotPublicKey, vector.PublicKeyBase64)
	}
	if signature.Value != vector.SignatureBase64 {
		t.Errorf("signature vector = %s, want %s", signature.Value, vector.SignatureBase64)
	}
}
