package gatewayprojection

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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
	user := &store.User{ID: "11111111-1111-4111-8111-111111111111", Name: "Gateway Device Owner", Email: "gateway-owner@example.com", Active: true}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
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
	if _, _, err := st.SetOverlayDeviceStatus(context.Background(), "11111111-1111-4111-8111-111111111111", "network-test", "dev_test_01", store.OverlayDeviceInactive, 1, "", &store.AuditLog{Action: store.AuditActionOverlayDeviceStateUpdate}); err != nil {
		t.Fatal(err)
	}
	service.config.AllowEmptyPeers = true
	if _, err := service.Project(context.Background(), "gw_test_01"); err == nil || !strings.Contains(err.Error(), "exceeds safety limit") {
		t.Fatalf("unsafe full removal accepted: %v", err)
	}
}

func TestInactiveUserIsRemovedFromGatewayPeers(t *testing.T) {
	st := store.NewMemoryStore()
	gatewayFixture(t, st)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	service := testService(t, st, now, 100)
	first, err := service.Project(context.Background(), "gw_test_01")
	if err != nil || len(first.WireGuard.Peers) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	user, err := st.GetUserByID(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	user.Active = false
	if err = st.UpdateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	service.config.AllowEmptyPeers = true
	second, err := service.Project(context.Background(), "gw_test_01")
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 || len(second.WireGuard.Peers) != 0 {
		t.Fatalf("inactive principal remained projected: %#v", second)
	}
}

func TestRevokedDeviceAdvancesSnapshotAndRemovesOldPeer(t *testing.T) {
	st := store.NewMemoryStore()
	gatewayFixture(t, st)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	service := testService(t, st, now, 100)
	service.config.AllowEmptyPeers = true
	first, err := service.Project(context.Background(), "gw_test_01")
	if err != nil || first.Generation != 1 || len(first.WireGuard.Peers) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	oldKey := first.WireGuard.Peers[0].PublicKey
	if _, _, err = st.SetOverlayDeviceStatus(context.Background(), "11111111-1111-4111-8111-111111111111", "network-test", "dev_test_01", store.OverlayDeviceRevoked, 1, "leave", &store.AuditLog{Action: store.AuditActionOverlayDeviceRevoke}); err != nil {
		t.Fatal(err)
	}
	second, err := service.Project(context.Background(), "gw_test_01")
	if err != nil || second.Generation != 2 || second.ExpectedPreviousGeneration != 1 || len(second.WireGuard.Peers) != 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	raw, _ := json.Marshal(second)
	if strings.Contains(string(raw), oldKey) {
		t.Fatalf("revoked key remained in snapshot: %s", raw)
	}
}
