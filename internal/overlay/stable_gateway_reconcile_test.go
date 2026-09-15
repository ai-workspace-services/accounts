package overlay

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newStableGatewayTestService(t *testing.T) (*Service, *gorm.DB, time.Time) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:stable-gateway-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return service, db, now
}

func seedStableGateway(t *testing.T, service *Service, owner string, now time.Time) {
	t.Helper()
	_, err := service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID: stableUATNetworkID, DisplayName: "UAT", CIDR: "10.77.0.0/24", GatewayID: stableUATGatewayID,
			GatewayWireGuardKey: base64KeyForTest(), GatewayWireGuardAddress: "10.77.0.1/32",
			GatewayEndpointHost: stableUATGatewayHost, GatewayEndpointPort: 443,
			TransportServerName: stableUATGatewayHost, TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", TransportKind: TransportVLESSXHTTP,
			TransportPath: DefaultTransportPath, TransportMode: DefaultTransportMode, TransportHost: stableUATGatewayHost,
			OwnerUserID: owner,
		},
		Invite: BootstrapInvite{DeviceID: "gateway-uat", Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}, "stable-test-token")
	if err != nil {
		t.Fatal(err)
	}
}

func base64KeyForTest() string {
	return "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="
}

func TestReconcileStableGatewayRepairsOnlyUATOwnerProjection(t *testing.T) {
	service, db, now := newStableGatewayTestService(t)
	seedStableGateway(t, service, "old-owner", now)
	if err := db.Create(&DeviceRecord{ID: "gateway-uat", UserUUID: "old-owner", UserID: "old-owner", NetworkID: stableUATNetworkID, Role: RoleGateway, Name: "Gateway", Platform: "linux", Hostname: stableUATGatewayHost, WireGuardPublicKey: base64KeyForTest(), WireGuardAddress: "10.77.0.1/32", Status: "active", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&RegistrationRecord{ID: "registration-uat", NetworkID: stableUATNetworkID, OwnerUserID: "old-owner", DeviceID: "one-uat", Platform: "linux", WireGuardPublicKey: base64KeyForTest(), WireGuardPublicKeyFingerprint: "fingerprint", TokenHash: "hash", Status: RegistrationStatusPending, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := service.ReconcileStableGateway(t.Context(), StableGatewayReconcileRequest{Environment: "uat", NetworkID: stableUATNetworkID, GatewayID: stableUATGatewayID, GatewayEndpointHost: stableUATGatewayHost, OwnerUserID: "new-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OwnerReconciled || result.PreviousOwnerID != "old-owner" || result.DeviceCount != 1 || result.RegistrationCount != 1 {
		t.Fatalf("unexpected reconciliation result: %#v", result)
	}

	var network NetworkRecord
	var device DeviceRecord
	var registration RegistrationRecord
	if err := db.First(&network, "id = ?", stableUATNetworkID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&device, "id = ?", "gateway-uat").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&registration, "id = ?", "registration-uat").Error; err != nil {
		t.Fatal(err)
	}
	if network.OwnerUserID != "new-owner" || device.UserID != "new-owner" || device.UserUUID != "new-owner" || registration.OwnerUserID != "new-owner" {
		t.Fatalf("owner projection not reconciled: network=%#v device=%#v registration=%#v", network, device, registration)
	}

	idempotent, err := service.ReconcileStableGateway(t.Context(), StableGatewayReconcileRequest{Environment: "uat", NetworkID: stableUATNetworkID, GatewayID: stableUATGatewayID, GatewayEndpointHost: stableUATGatewayHost, OwnerUserID: "new-owner"})
	if err != nil || idempotent.OwnerReconciled {
		t.Fatalf("expected idempotent no-op, result=%#v err=%v", idempotent, err)
	}
}

func TestReconcileStableGatewayRejectsNonUATOrMismatchedIdentity(t *testing.T) {
	service, db, now := newStableGatewayTestService(t)
	seedStableGateway(t, service, "owner", now)
	cases := []StableGatewayReconcileRequest{
		{Environment: "prod", NetworkID: stableUATNetworkID, GatewayID: stableUATGatewayID, GatewayEndpointHost: stableUATGatewayHost, OwnerUserID: "owner"},
		{Environment: "uat", NetworkID: "net_prod", GatewayID: stableUATGatewayID, GatewayEndpointHost: stableUATGatewayHost, OwnerUserID: "owner"},
		{Environment: "uat", NetworkID: stableUATNetworkID, GatewayID: "gw-other", GatewayEndpointHost: stableUATGatewayHost, OwnerUserID: "owner"},
		{Environment: "uat", NetworkID: stableUATNetworkID, GatewayID: stableUATGatewayID, GatewayEndpointHost: "other.svc.plus", OwnerUserID: "owner"},
	}
	for _, request := range cases {
		if _, err := service.ReconcileStableGateway(t.Context(), request); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("request %#v returned %v, want ErrInvalidInput", request, err)
		}
	}
	var network NetworkRecord
	if err := db.First(&network, "id = ?", stableUATNetworkID).Error; err != nil {
		t.Fatal(err)
	}
	if network.OwnerUserID != "owner" {
		t.Fatalf("rejected request changed owner: %#v", network)
	}
}

func TestReconcileStableGatewayRequiresExistingNetwork(t *testing.T) {
	service, _, _ := newStableGatewayTestService(t)
	_, err := service.ReconcileStableGateway(t.Context(), StableGatewayReconcileRequest{Environment: "uat", NetworkID: stableUATNetworkID, GatewayID: stableUATGatewayID, GatewayEndpointHost: stableUATGatewayHost, OwnerUserID: "owner"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
