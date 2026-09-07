package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAdminResourcesAreIsolatedByOwner(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:admin-isolation-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-isolation", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	seedOwnedNetwork := func(owner, networkID, cidr, gatewayID string) {
		t.Helper()
		_, err := service.Seed(t.Context(), BootstrapConfig{
			Network: BootstrapNetwork{
				ID: networkID, DisplayName: networkID, CIDR: cidr, GatewayID: gatewayID,
				GatewayWireGuardKey:     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
				GatewayWireGuardAddress: "10.99.0.1/32", GatewayEndpointHost: gatewayID + ".example.test",
				GatewayEndpointPort: 51820, TransportServerName: gatewayID + ".example.test", TransportPort: 443,
				TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: owner,
			},
			Invite: BootstrapInvite{DeviceID: "one-" + owner, Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)},
		}, "token-"+owner)
		if err != nil {
			t.Fatal(err)
		}
	}
	seedOwnedNetwork("user-a", "network-a", "10.91.0.0/29", "gateway-a")
	seedOwnedNetwork("user-b", "network-b", "10.92.0.0/29", "gateway-b")

	devices := []DeviceRecord{
		{ID: "device-a", UserID: "user-a", NetworkID: "network-a", Role: RoleOne, Name: "A", Platform: "linux", Hostname: "a", WireGuardPublicKey: "key-a", WireGuardAddress: "10.91.0.2/32", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: "device-b", UserID: "user-b", NetworkID: "network-b", Role: RoleGateway, Name: "B", Platform: "linux", Hostname: "b", WireGuardPublicKey: "key-b", WireGuardAddress: "10.92.0.2/32", Status: "active", CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&devices).Error; err != nil {
		t.Fatal(err)
	}

	overview, err := service.AdminOverview(t.Context(), "user-a")
	if err != nil || overview.NetworkCount != 1 || overview.DeviceCount != 1 || overview.GatewayCount != 0 {
		t.Fatalf("user A overview leaked resources: %#v err=%v", overview, err)
	}
	networks, err := service.AdminNetworks(t.Context(), "user-a")
	if err != nil || len(networks) != 1 || networks[0].ID != "network-a" {
		t.Fatalf("user A networks leaked resources: %#v err=%v", networks, err)
	}
	listedDevices, err := service.AdminDevices(t.Context(), "user-a")
	if err != nil || len(listedDevices) != 1 || listedDevices[0].ID != "device-a" {
		t.Fatalf("user A devices leaked resources: %#v err=%v", listedDevices, err)
	}
	invites, err := service.AdminInvites(t.Context(), "user-a")
	if err != nil || len(invites) != 1 || invites[0].NetworkID != "network-a" {
		t.Fatalf("user A invites leaked resources: %#v err=%v", invites, err)
	}
	if _, err := service.AdminPolicy(t.Context(), "user-a", "network-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user A read user B policy: %v", err)
	}
	if _, err := service.AdminUpdatePolicy(t.Context(), "user-a", "network-b", PolicyArtifact{DefaultAction: "deny"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user A updated user B policy: %v", err)
	}
	if err := service.AdminRevokeDevice(t.Context(), "user-a", "device-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user A revoked user B device: %v", err)
	}
	var deviceB DeviceRecord
	if err := db.First(&deviceB, "id = ?", "device-b").Error; err != nil || deviceB.Status != "active" {
		t.Fatalf("user B device changed: %#v err=%v", deviceB, err)
	}
}

func TestBootstrapCannotTakeOverAnotherUsersNetwork(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:bootstrap-isolation-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, Config{SigningPrivateKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	network := BootstrapNetwork{ID: "shared-id", DisplayName: "A", CIDR: "10.93.0.0/29", GatewayID: "gateway-a", GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), GatewayWireGuardAddress: "10.93.0.1/32", GatewayEndpointHost: "gateway-a.example.test", GatewayEndpointPort: 51820, TransportServerName: "gateway-a.example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "user-a"}
	if _, err := service.Seed(t.Context(), BootstrapConfig{Network: network, Invite: BootstrapInvite{Platform: "linux", Role: RoleOne, ExpiresAt: time.Now().UTC().Add(time.Hour)}}, "owner-a-token"); err != nil {
		t.Fatal(err)
	}
	network.OwnerUserID = "user-b"
	_, err = service.Seed(t.Context(), BootstrapConfig{
		Network: network,
		Invite:  BootstrapInvite{Platform: "linux", Role: RoleOne, ExpiresAt: time.Now().UTC().Add(time.Hour)},
	}, "takeover-token")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected cross-user bootstrap rejection, got %v", err)
	}
}
