package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
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
	if err != nil || overview.NetworkCount != 1 || overview.DeviceCount != 1 || overview.GatewayCount != 0 || overview.OneCount != 1 || overview.OneStatus != "active" || overview.GatewayStatus != "pending" {
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

func TestAdminDeleteNetworkRequiresOwnershipAndRemovesScopedData(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:admin-delete-network-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
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
	seed := func(owner, id, cidr string) {
		t.Helper()
		_, err := service.Seed(t.Context(), BootstrapConfig{
			Network: BootstrapNetwork{
				ID: id, DisplayName: id, CIDR: cidr, GatewayID: "gateway-" + id,
				GatewayWireGuardKey:     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
				GatewayWireGuardAddress: "10.98.0.1/32", GatewayEndpointHost: "gateway.example.test",
				GatewayEndpointPort: 443, TransportServerName: "gateway.example.test", TransportPort: 443,
				TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: owner,
			},
			Invite: BootstrapInvite{DeviceID: "gateway-" + id, Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
		}, "token-"+id)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed("owner-a", "net-delete", "10.98.0.0/29")
	seed("owner-b", "net-keep", "10.99.0.0/29")
	target := DeviceRecord{ID: "device-delete", UserUUID: "11111111-2222-4333-8444-555555555555", UserID: "owner-a", NetworkID: "net-delete", Role: RoleOne, Name: "target", Platform: "linux", Hostname: "target", WireGuardPublicKey: "public-target", WireGuardAddress: "10.98.0.2/32", Status: "active", CreatedAt: now, UpdatedAt: now}
	other := DeviceRecord{ID: "device-keep", UserUUID: "22222222-3333-4444-8555-666666666666", UserID: "owner-b", NetworkID: "net-keep", Role: RoleOne, Name: "other", Platform: "linux", Hostname: "other", WireGuardPublicKey: "public-other", WireGuardAddress: "10.99.0.2/32", Status: "active", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&[]DeviceRecord{target, other}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&CredentialRecord{ID: "credential-delete", DeviceID: target.ID, CredentialID: "credential-id-delete", TokenHash: HashSecret("credential-token-delete"), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&EnrollmentRecord{ID: "enrollment-delete", DeviceID: target.ID, TokenHash: HashSecret("enrollment-token-delete"), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AckRecord{ID: "ack-delete", DeviceID: target.ID, NetworkID: "net-delete", Generation: 1, ConfigID: "config-delete", AppliedAt: now, ReceivedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&RegistrationRecord{ID: "registration-delete", NetworkID: "net-delete", OwnerUserID: "owner-a", DeviceID: "pending", Platform: "linux", WireGuardPublicKey: "pending-key", WireGuardPublicKeyFingerprint: "fingerprint", TokenHash: HashSecret("registration-token-delete"), Status: RegistrationStatusPending, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	if err := service.AdminDeleteNetwork(t.Context(), "owner-b", "net-delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner was able to delete the network: %v", err)
	}
	if err := service.AdminDeleteNetwork(t.Context(), "owner-a", "net-delete"); err != nil {
		t.Fatalf("delete owned network: %v", err)
	}
	for _, model := range []any{&NetworkRecord{}, &InviteRecord{}, &DeviceRecord{}, &CredentialRecord{}, &EnrollmentRecord{}, &AckRecord{}, &RegistrationRecord{}} {
		query := db.Model(model)
		switch model.(type) {
		case *NetworkRecord:
			query = query.Where("id = ?", "net-delete")
		case *InviteRecord, *AckRecord, *RegistrationRecord:
			query = query.Where("network_id = ?", "net-delete")
		case *DeviceRecord:
			query = query.Where("network_id = ?", "net-delete")
		case *CredentialRecord, *EnrollmentRecord:
			query = query.Where("device_id = ?", target.ID)
		}
		var count int64
		if err := query.Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("network-scoped records remain for %T: count=%d err=%v", model, count, err)
		}
	}
	if err := db.First(&NetworkRecord{}, "id = ?", "net-keep").Error; err != nil {
		t.Fatalf("another owner's network was removed: %v", err)
	}
	if err := db.First(&DeviceRecord{}, "id = ?", other.ID).Error; err != nil {
		t.Fatalf("another owner's device was removed: %v", err)
	}
}

func TestAdminDeleteNetworkRejectsAmbiguousDeviceCredentialOwnership(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:admin-delete-network-shared-id-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
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
	now := time.Now().UTC()
	for _, item := range []struct{ owner, id, cidr string }{{"owner-a", "net-a", "10.97.0.0/29"}, {"owner-b", "net-b", "10.96.0.0/29"}} {
		_, err := service.Seed(t.Context(), BootstrapConfig{
			Network: BootstrapNetwork{ID: item.id, DisplayName: item.id, CIDR: item.cidr, GatewayID: "gw-" + item.id, GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)), GatewayWireGuardAddress: "10.96.0.1/32", GatewayEndpointHost: "gw.example.test", GatewayEndpointPort: 443, TransportServerName: "gw.example.test", TransportPort: 443, TransportAuthID: "22222222-1111-4111-8111-111111111111", OwnerUserID: item.owner},
			Invite:  BootstrapInvite{DeviceID: "gw-" + item.id, Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
		}, "seed-"+item.id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&[]DeviceRecord{
		{ID: "same-device", UserUUID: "11111111-2222-4333-8444-555555555555", UserID: "owner-a", NetworkID: "net-a", Role: RoleOne, Name: "A", Platform: "linux", Hostname: "a", WireGuardPublicKey: "key-a", WireGuardAddress: "10.97.0.2/32", Status: "active"},
		{ID: "same-device", UserUUID: "22222222-3333-4444-8555-666666666666", UserID: "owner-b", NetworkID: "net-b", Role: RoleOne, Name: "B", Platform: "linux", Hostname: "b", WireGuardPublicKey: "key-b", WireGuardAddress: "10.96.0.2/32", Status: "active"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.AdminDeleteNetwork(t.Context(), "owner-a", "net-a"); !errors.Is(err, ErrDeviceConflict) {
		t.Fatalf("expected conflict for ambiguous child device IDs, got %v", err)
	}
	if err := db.First(&NetworkRecord{}, "id = ?", "net-a").Error; err != nil {
		t.Fatalf("ambiguous deletion removed target network: %v", err)
	}
}

func TestAdminDevicesProjectionReportsACKStateAndOwnerScope(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:admin-device-projection-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
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

	seedNetwork := func(owner, networkID, cidr, gatewayID, gatewayAddress, token string) {
		t.Helper()
		_, err := service.Seed(t.Context(), BootstrapConfig{
			Network: BootstrapNetwork{ID: networkID, DisplayName: networkID, CIDR: cidr, GatewayID: gatewayID, GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), GatewayWireGuardAddress: gatewayAddress, GatewayEndpointHost: gatewayID + ".example.test", GatewayEndpointPort: 51820, TransportServerName: gatewayID + ".example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: owner},
			Invite:  BootstrapInvite{DeviceID: "seed-" + owner, Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)},
		}, token)
		if err != nil {
			t.Fatal(err)
		}
	}
	seedNetwork("user-a", "network-a", "10.91.0.0/28", "gateway-a", "10.91.0.1/32", "seed-a")
	seedNetwork("user-b", "network-b", "10.92.0.0/28", "gateway-b", "10.92.0.1/32", "seed-b")
	if err := db.Model(&NetworkRecord{}).Where("id = ?", "network-a").Update("config_generation", uint64(2)).Error; err != nil {
		t.Fatal(err)
	}

	recent := now.Add(-time.Minute)
	stale := now.Add(-6 * time.Minute)
	future := now.Add(time.Minute)
	devices := []DeviceRecord{
		{ID: "active-recent", UserID: "user-a", NetworkID: "network-a", Role: RoleOne, Name: "Recent", Platform: "linux", Hostname: "recent", WireGuardPublicKey: "key-recent", WireGuardAddress: "10.91.0.2/32", Status: "active", LastSeenAt: &recent, CreatedAt: now, UpdatedAt: now},
		{ID: "active-stale", UserID: "user-a", NetworkID: "network-a", Role: RoleOne, Name: "Stale", Platform: "linux", Hostname: "stale", WireGuardPublicKey: "key-stale", WireGuardAddress: "10.91.0.3/32", Status: "active", LastSeenAt: &stale, CreatedAt: now, UpdatedAt: now},
		{ID: "active-old-generation", UserID: "user-a", NetworkID: "network-a", Role: RoleOne, Name: "Old generation", Platform: "linux", Hostname: "old-generation", WireGuardPublicKey: "key-old", WireGuardAddress: "10.91.0.4/32", Status: "active", LastSeenAt: &recent, CreatedAt: now, UpdatedAt: now},
		{ID: "revoked-gateway", UserID: "user-a", NetworkID: "network-a", Role: RoleGateway, Name: "Revoked gateway", Platform: "linux", Hostname: "revoked", WireGuardPublicKey: "key-revoked", WireGuardAddress: "10.91.0.1/32", Status: "revoked", LastSeenAt: &recent, CreatedAt: now, UpdatedAt: now},
		{ID: "active-future-ack", UserID: "user-a", NetworkID: "network-a", Role: RoleOne, Name: "Future ACK", Platform: "linux", Hostname: "future", WireGuardPublicKey: "key-future", WireGuardAddress: "10.91.0.5/32", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: "foreign-device", UserID: "user-b", NetworkID: "network-b", Role: RoleGateway, Name: "Foreign", Platform: "linux", Hostname: "foreign", WireGuardPublicKey: "key-foreign", WireGuardAddress: "10.92.0.1/32", Status: "active", CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&devices).Error; err != nil {
		t.Fatal(err)
	}
	acks := []AckRecord{
		{ID: "ack-recent", DeviceID: "active-recent", NetworkID: "network-a", Generation: 2, ConfigID: configID("network-a", "active-recent", 2), AppliedAt: recent, ReceivedAt: recent},
		{ID: "ack-stale", DeviceID: "active-stale", NetworkID: "network-a", Generation: 2, ConfigID: configID("network-a", "active-stale", 2), AppliedAt: stale, ReceivedAt: stale},
		{ID: "ack-old-generation", DeviceID: "active-old-generation", NetworkID: "network-a", Generation: 1, ConfigID: configID("network-a", "active-old-generation", 1), AppliedAt: recent, ReceivedAt: recent},
		{ID: "ack-revoked", DeviceID: "revoked-gateway", NetworkID: "network-a", Generation: 2, ConfigID: configID("network-a", "revoked-gateway", 2), AppliedAt: recent, ReceivedAt: recent},
		{ID: "ack-future", DeviceID: "active-future-ack", NetworkID: "network-a", Generation: 2, ConfigID: configID("network-a", "active-future-ack", 2), AppliedAt: future, ReceivedAt: future},
	}
	if err := db.Create(&acks).Error; err != nil {
		t.Fatal(err)
	}

	listed, err := service.AdminDevices(t.Context(), "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 5 {
		t.Fatalf("expected five owner-scoped devices, got %#v", listed)
	}
	byID := make(map[string]AdminDevice, len(listed))
	for _, device := range listed {
		byID[device.ID] = device
	}
	want := map[string]struct {
		role, status, connectionStatus string
	}{
		"active-recent":         {RoleOne, "active", "recent_ack"},
		"active-stale":          {RoleOne, "active", "stale"},
		"active-old-generation": {RoleOne, "active", "never_seen"},
		"revoked-gateway":       {RoleGateway, "revoked", "revoked"},
		"active-future-ack":     {RoleOne, "active", "stale"},
	}
	for id, expected := range want {
		device, ok := byID[id]
		if !ok || device.Role != expected.role || device.Status != expected.status || device.ConnectionStatus != expected.connectionStatus {
			t.Fatalf("unexpected admin device %q: got %#v want role=%q status=%q connection_status=%q", id, device, expected.role, expected.status, expected.connectionStatus)
		}
	}
	if byID["active-old-generation"].LastSeenAt == nil || byID["revoked-gateway"].LastSeenAt == nil {
		t.Fatal("admin projection should preserve nullable last_seen_at values")
	}

	foreign, err := service.AdminDevices(t.Context(), "user-b")
	if err != nil || len(foreign) != 1 || foreign[0].ID != "foreign-device" {
		t.Fatalf("owner B projection leaked resources: %#v err=%v", foreign, err)
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

func TestAdminCreateInviteIsOwnerScopedAndDeviceBound(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:admin-create-invite-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
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
	_, err = service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{ID: "network-owner-a", DisplayName: "Owner A", CIDR: "10.94.0.0/29", GatewayID: "gateway-a", GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), GatewayWireGuardAddress: "10.94.0.1/32", GatewayEndpointHost: "gateway-a.example.test", GatewayEndpointPort: 51820, TransportServerName: "gateway-a.example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "user-a"},
		Invite:  BootstrapInvite{DeviceID: "linux-a", Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)},
	}, "seed-owner-a")
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.AdminCreateInvite(t.Context(), "user-a", "https://accounts.example.test", AdminInviteRequest{NetworkID: "network-owner-a", DeviceID: "mac-owner-a", Platform: "darwin", Role: RoleOne, ExpiresAt: now.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("create owner invite: %v", err)
	}
	if result.Invite.NetworkID != "network-owner-a" || result.Invite.DeviceID != "mac-owner-a" || result.Invite.Platform != "darwin" || result.Invite.RemainingUses != 1 {
		t.Fatalf("unexpected invite summary: %#v", result.Invite)
	}
	if !strings.HasPrefix(result.JoinURI, "xconnect://join/xjt_") || !strings.Contains(result.JoinURI, "controller=https%3A%2F%2Faccounts.example.test") {
		t.Fatalf("unexpected join URI shape: %q", result.JoinURI)
	}
	if _, err := service.AdminCreateInvite(t.Context(), "user-b", "https://accounts.example.test", AdminInviteRequest{NetworkID: "network-owner-a", DeviceID: "mac-owner-b", Platform: "darwin", Role: RoleOne, ExpiresAt: now.Add(30 * time.Minute)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner invite creation should be hidden, got %v", err)
	}
	var stored InviteRecord
	if err := db.Where("id = ?", result.Invite.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.JoinURI, stored.TokenHash) || stored.TokenHash == "" {
		t.Fatalf("invite persistence exposed raw token: %#v", stored)
	}
}
