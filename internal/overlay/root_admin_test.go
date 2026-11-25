package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRootAdminCanManageAllOwnedResources(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:root-admin-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-root", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID: "net-owner-a", DisplayName: "Owner A Net", CIDR: "10.91.0.0/16", GatewayID: "gw-a",
			GatewayWireGuardKey:     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)),
			GatewayWireGuardAddress: "10.91.0.1/32", GatewayEndpointHost: "gw-a.example.test",
			GatewayEndpointPort: 51820, TransportServerName: "gw-a.example.test", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "user-a",
		},
		Invite: BootstrapInvite{DeviceID: "one-a", Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)},
	}, "token-a")
	if err != nil {
		t.Fatal(err)
	}

	rootCtx := WithRootAdmin(t.Context())

	// Non-root user-b sees nothing.
	if nets, err := service.AdminNetworks(t.Context(), "user-b"); err != nil || len(nets) != 0 {
		t.Fatalf("expected 0 networks for non-root user-b, got %d (err=%v)", len(nets), err)
	}

	// Root admin user-b sees and manages user-a's network.
	nets, err := service.AdminNetworks(rootCtx, "user-b")
	if err != nil || len(nets) != 1 || nets[0].ID != "net-owner-a" {
		t.Fatalf("expected root admin to see net-owner-a, got %+v (err=%v)", nets, err)
	}

	if _, err := service.AdminOverview(rootCtx, "user-b"); err != nil {
		t.Fatalf("expected root admin overview to succeed, got err=%v", err)
	}

	if _, err := service.AdminUpdateNetworkMode(rootCtx, "user-b", "net-owner-a", ConnectionModeWGUDPL3); err != nil {
		t.Fatalf("root admin update network mode failed: %v", err)
	}

	if _, err := service.AdminBindNetworkMember(rootCtx, "user-b", "net-owner-a", "user-c", "c@example.test"); err != nil {
		t.Fatalf("root admin bind member failed: %v", err)
	}
	if members, err := service.AdminNetworkMembers(rootCtx, "user-b", "net-owner-a"); err != nil || len(members) != 1 {
		t.Fatalf("expected 1 member, got %+v (err=%v)", members, err)
	}
	if err := service.AdminRemoveNetworkMember(rootCtx, "user-b", "net-owner-a", "user-c"); err != nil {
		t.Fatalf("root admin remove member failed: %v", err)
	}

	if _, err := service.AdminCreateInvite(rootCtx, "user-b", "https://accounts.example.test", AdminInviteRequest{
		NetworkID: "net-owner-a", DeviceID: "one-extra", Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("root admin create invite failed: %v", err)
	}
	if invites, err := service.AdminInvites(rootCtx, "user-b"); err != nil || len(invites) != 2 {
		t.Fatalf("expected 2 invites for root admin, got %d (err=%v)", len(invites), err)
	}

	if err := service.AdminDeleteNetwork(rootCtx, "user-b", "net-owner-a"); err != nil {
		t.Fatalf("root admin delete network failed: %v", err)
	}
	if nets, err := service.AdminNetworks(rootCtx, "user-b"); err != nil || len(nets) != 0 {
		t.Fatalf("expected 0 networks after root admin delete, got %d (err=%v)", len(nets), err)
	}
}
