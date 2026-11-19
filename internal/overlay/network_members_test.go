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

func TestAdminNetworkMembersBindingLifecycle(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:network-members-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-members", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32))
	_, err = service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID:                      "net-ph-xconnect",
			DisplayName:             "PH XConnect",
			CIDR:                    "10.88.0.0/29",
			GatewayID:               "ph-xconnect-gateway-01",
			GatewayWireGuardKey:     gwKey,
			GatewayWireGuardAddress: "10.88.0.1/32",
			GatewayEndpointHost:     "ph.example.test",
			GatewayEndpointPort:     51820,
			TransportServerName:     "ph.example.test",
			TransportPort:           443,
			TransportAuthID:         "11111111-1111-1111-1111-111111111111",
			OwnerUserID:             "owner-1",
		},
		Invite: BootstrapInvite{
			DeviceID:  "ph-xconnect-gateway-01",
			Platform:  "linux",
			Role:      RoleGateway,
			ExpiresAt: now.Add(time.Hour),
		},
	}, "seed-ph-token")
	if err != nil {
		t.Fatal(err)
	}

	// Non-owner cannot list, bind, or remove members.
	if _, err := service.AdminNetworkMembers(t.Context(), "other-admin", "net-ph-xconnect"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign AdminNetworkMembers, got %v", err)
	}
	if _, err := service.AdminBindNetworkMember(t.Context(), "other-admin", "net-ph-xconnect", "member-1", "156405189@qq.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign AdminBindNetworkMember, got %v", err)
	}
	if err := service.AdminRemoveNetworkMember(t.Context(), "other-admin", "net-ph-xconnect", "member-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign AdminRemoveNetworkMember, got %v", err)
	}

	// Unbound member cannot discover gateway or enroll.
	gateways, err := service.UserGateways(t.Context(), "member-1")
	if err != nil || len(gateways) != 0 {
		t.Fatalf("expected no gateways before binding, got %#v err=%v", gateways, err)
	}
	oneKey1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{21}, 32))
	if _, err := service.UserEnrollDevice(t.Context(), "member-1", UserEnrollRequest{
		NetworkID:          "net-ph-xconnect",
		GatewayID:          "ph-xconnect-gateway-01",
		DeviceID:           "member-one-1",
		Platform:           "darwin",
		WireGuardPublicKey: oneKey1,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden before binding, got %v", err)
	}

	// Owner binds member-1; second call is idempotent.
	bound1, err := service.AdminBindNetworkMember(t.Context(), "owner-1", "net-ph-xconnect", "member-1", "156405189@qq.com")
	if err != nil {
		t.Fatalf("bind member: %v", err)
	}
	bound2, err := service.AdminBindNetworkMember(t.Context(), "owner-1", "net-ph-xconnect", "member-1", "156405189@qq.com")
	if err != nil {
		t.Fatalf("idempotent bind member: %v", err)
	}
	if bound1.ID == "" || bound1.ID != bound2.ID || bound1.NetworkID != "net-ph-xconnect" || bound1.UserID != "member-1" || bound1.Email != "156405189@qq.com" {
		t.Fatalf("unexpected member binding: bound1=%#v bound2=%#v", bound1, bound2)
	}

	members, err := service.AdminNetworkMembers(t.Context(), "owner-1", "net-ph-xconnect")
	if err != nil || len(members) != 1 || members[0].UserID != "member-1" {
		t.Fatalf("unexpected members list: %#v err=%v", members, err)
	}

	// Bound member discovers gateway and enrolls device.
	gateways, err = service.UserGateways(t.Context(), "member-1")
	if err != nil || len(gateways) != 1 || gateways[0].NetworkID != "net-ph-xconnect" || gateways[0].GatewayID != "ph-xconnect-gateway-01" {
		t.Fatalf("expected bound gateway, got %#v err=%v", gateways, err)
	}
	enrolled, err := service.UserEnrollDevice(t.Context(), "member-1", UserEnrollRequest{
		NetworkID:          "net-ph-xconnect",
		GatewayID:          "ph-xconnect-gateway-01",
		DeviceID:           "member-one-1",
		Name:               "Member Mac",
		Platform:           "darwin",
		Hostname:           "member-mac.local",
		WireGuardPublicKey: oneKey1,
	})
	if err != nil {
		t.Fatalf("bound member enroll failed: %v", err)
	}
	if enrolled.Device.UserID != "member-1" || enrolled.Device.NetworkID != "net-ph-xconnect" || !strings.HasPrefix(enrolled.DeviceCredential.Credential, "xdc_") {
		t.Fatalf("unexpected member enroll response: %#v", enrolled)
	}

	// Bound member can fetch signed config for their enrolled device, and owner sees it in AdminDevices.
	if _, _, err := service.UserConfig(t.Context(), "member-1", "member-one-1", "net-ph-xconnect", true); err != nil {
		t.Fatalf("member UserConfig failed: %v", err)
	}
	adminDevices, err := service.AdminDevices(t.Context(), "owner-1")
	if err != nil || len(adminDevices) != 1 || adminDevices[0].ID != "member-one-1" {
		t.Fatalf("owner AdminDevices missing member device: %#v err=%v", adminDevices, err)
	}

	// Unbind member; member can no longer enroll new devices.
	if err := service.AdminRemoveNetworkMember(t.Context(), "owner-1", "net-ph-xconnect", "member-1"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if err := service.AdminRemoveNetworkMember(t.Context(), "owner-1", "net-ph-xconnect", "member-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound removing already-unbound member, got %v", err)
	}
	oneKey2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{22}, 32))
	if _, err := service.UserEnrollDevice(t.Context(), "member-1", UserEnrollRequest{
		NetworkID:          "net-ph-xconnect",
		GatewayID:          "ph-xconnect-gateway-01",
		DeviceID:           "member-one-2",
		Platform:           "darwin",
		WireGuardPublicKey: oneKey2,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden after unbinding, got %v", err)
	}
}
