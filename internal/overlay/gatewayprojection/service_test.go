package gatewayprojection

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"account/internal/overlay/projection"
	"account/internal/store"
)

func testService(t *testing.T, st store.Store, now time.Time, maxRemoval float64) *Service {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	signer, err := projection.NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "key_test_01")
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now }
	signed, err := projection.NewService(projection.NewMemoryRepository(clock), signer, clock, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(st, signed, clock, Config{Lifetime: time.Hour, RenewalInterval: 30 * time.Minute, MaxPeerRemovalPercent: maxRemoval})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func gatewayFixture(t *testing.T, st store.Store) {
	t.Helper()
	node := &store.OverlayNode{ID: "gw_test_01", NetworkID: "network-test", Name: "Gateway", Role: "gateway", WireGuardPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), WireGuardAddress: "10.77.0.1/32", EndpointHost: "gateway.example", EndpointPort: 443, TransportType: "vless-tls", TransportSecurity: "tls", TransportUUID: "11111111-1111-4111-8111-111111111111", Healthy: true}
	if err := st.UpsertOverlayNode(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	device := &store.OverlayDevice{ID: "dev_test_01", UserID: "11111111-1111-4111-8111-111111111111", NetworkID: "network-test", Name: "device", Platform: "linux", WireGuardPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), WireGuardAddress: "10.77.0.10/32"}
	if err := st.UpsertOverlayDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
}

func TestProjectIsConcurrentIdempotentAndSecretFree(t *testing.T) {
	st := store.NewMemoryStore()
	gatewayFixture(t, st)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	service := testService(t, st, now, 100)
	const workers = 16
	results := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot, err := service.Project(context.Background(), "gw_test_01")
			if err != nil {
				errs <- err
				return
			}
			results <- snapshot.SnapshotID
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id string
	for value := range results {
		if id == "" {
			id = value
		} else if value != id {
			t.Fatalf("same source produced different snapshots: %s %s", id, value)
		}
	}
	record, err := st.GetLatestOverlayGatewaySnapshot(context.Background(), "gw_test_01")
	if err != nil || record.Generation != 1 {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	text := string(record.SignedPayload)
	if strings.Contains(text, "11111111-1111-4111-8111-111111111111") || strings.Contains(text, "transport_uuid") || strings.Contains(text, "auth_id") {
		t.Fatalf("snapshot leaked relay credential: %s", text)
	}
	if !strings.Contains(text, `"transport":"vless-tls-xudp"`) || !strings.Contains(text, `"proxy_core":"xray"`) {
		t.Fatalf("runtime contract drifted: %s", text)
	}
}

func TestProjectSafetyRejectsRemovalBeyondSignedLimit(t *testing.T) {
	st := store.NewMemoryStore()
	gatewayFixture(t, st)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	service := testService(t, st, now, 20)
	if _, err := service.Project(context.Background(), "gw_test_01"); err != nil {
		t.Fatal(err)
	}
	device := &store.OverlayDevice{ID: "dev_test_01", UserID: "11111111-1111-4111-8111-111111111111", NetworkID: "other", Name: "device", Platform: "linux", WireGuardPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), WireGuardAddress: "10.77.0.10/32"}
	if err := st.UpsertOverlayDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	service.config.AllowEmptyPeers = true
	if _, err := service.Project(context.Background(), "gw_test_01"); err == nil || !strings.Contains(err.Error(), "exceeds safety limit") {
		t.Fatalf("unsafe full removal accepted: %v", err)
	}
}
