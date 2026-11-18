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

	"account/internal/agentproto"
)

func TestRecordRuntimeObservationPersistsGatewayAndPeerTelemetry(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:telemetry-persist-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-telemetry", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	_, err = service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID: "net_uat", DisplayName: "UAT Network", CIDR: "10.77.0.0/24", GatewayID: "gw-uat-tw-xconnect",
			GatewayWireGuardKey: gwKey, GatewayWireGuardAddress: "10.77.0.1/32",
			GatewayEndpointHost: "tw-xconnect.svc.plus", GatewayEndpointPort: 51820,
			TransportServerName: "tw-xconnect.svc.plus", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "owner-1",
		},
		Invite: BootstrapInvite{DeviceID: "gw-uat-tw-xconnect", Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}, "seed-token")
	if err != nil {
		t.Fatal(err)
	}

	devices := []DeviceRecord{
		{ID: "gw-uat-tw-xconnect", UserUUID: "owner-1", UserID: "owner-1", NetworkID: "net_uat", Role: RoleGateway, Name: "TW Gateway", Platform: "linux", Hostname: "tw-xconnect.svc.plus", WireGuardPublicKey: gwKey, WireGuardAddress: "10.77.0.1/32", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: "one-macbook", UserUUID: "owner-1", UserID: "owner-1", NetworkID: "net_uat", Role: RoleOne, Name: "MacBook Pro", Platform: "darwin", Hostname: "macbook", WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)), WireGuardAddress: "10.77.0.2/32", Status: "active", CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&devices).Error; err != nil {
		t.Fatal(err)
	}

	handshakeAt := now.Add(-15 * time.Second)
	err = service.RecordRuntimeObservation(t.Context(), RuntimeObservationRequest{
		NetworkID: "net_uat",
		DeviceID:  "gw-uat-tw-xconnect",
		Role:      RoleGateway,
		Healthy:   true,
		GatewayTelemetry: &agentproto.GatewayTelemetry{
			SchemaVersion:     1,
			NetworkID:         "net_uat",
			GatewayID:         "gw-uat-tw-xconnect",
			EndpointHost:      "tw-xconnect.svc.plus",
			ConnectionMode:    ConnectionModeWGVLESSL3,
			AppliedConfigID:   "cfg_test",
			AppliedGeneration: 1,
			WireGuardRunning:  true,
			XrayRunning:       true,
			RelayRunning:      true,
			SampledAt:         now,
			ExpiresAt:         now.Add(5 * time.Minute),
			Traffic: agentproto.GatewayTrafficSummary{
				RxBytesTotal:    10000,
				TxBytesTotal:    20000,
				InternetRxBytes: 6000,
				InternetTxBytes: 12000,
				PrivateRxBytes:  4000,
				PrivateTxBytes:  8000,
			},
			Peers: []agentproto.GatewayPeerTelemetry{
				{
					DeviceID:          "one-macbook",
					WireGuardAddress:  "10.77.0.2/32",
					Connected:         true,
					Path:              "direct",
					Endpoint:          "198.51.100.10:51820",
					LatestHandshakeAt: &handshakeAt,
					RxBytes:           4000,
					TxBytes:           8000,
					RTTMillis:         18,
				},
				{
					DeviceID:  "unknown-foreign-peer",
					Connected: true,
					RxBytes:   99999,
				},
			},
			TopApps: []agentproto.GatewayAppTraffic{
				{App: "internal-git.svc.plus:443", RxBytes: 2500, TxBytes: 5000, Connections: 12},
				{App: "k8s-api.internal:6443", RxBytes: 1500, TxBytes: 3000, Connections: 4},
			},
		},
	})
	if err != nil {
		t.Fatalf("record runtime observation: %v", err)
	}

	var telemetryCount int64
	if err := db.Model(&DeviceTelemetryRecord{}).Count(&telemetryCount).Error; err != nil || telemetryCount != 2 {
		t.Fatalf("expected 2 telemetry records (gateway + active peer), got %d err=%v", telemetryCount, err)
	}

	overview, err := service.AdminOverview(t.Context(), "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if overview.GatewayStatus != "connected" || overview.OneStatus != "connected" {
		t.Fatalf("expected connected overview status after runtime observation, got %#v", overview)
	}

	adminDevices, err := service.AdminDevices(t.Context(), "owner-1")
	if err != nil || len(adminDevices) != 2 {
		t.Fatalf("admin devices=%#v err=%v", adminDevices, err)
	}
	for _, dev := range adminDevices {
		if dev.ConnectionStatus != "recent_ack" || dev.RuntimeHealthy == nil || !*dev.RuntimeHealthy {
			t.Fatalf("expected device %s to be recent_ack and runtime_healthy, got %#v", dev.ID, dev)
		}
		if dev.ID == "one-macbook" {
			if dev.ActivePath != "direct" || dev.RTTMillis == nil || *dev.RTTMillis != 18 || dev.RxBytes != 4000 || dev.TxBytes != 8000 {
				t.Fatalf("unexpected peer telemetry on AdminDevice: %#v", dev)
			}
		}
	}

	telemetrySummary, err := service.AdminTelemetry(t.Context(), "owner-1")
	if err != nil {
		t.Fatalf("admin telemetry: %v", err)
	}
	if telemetrySummary.Status != "available" ||
		telemetrySummary.SASETraffic.RxBytesTotal != 10000 ||
		telemetrySummary.SASETraffic.PrivateTxBytes != 8000 ||
		telemetrySummary.ConnectedUsers.ConnectedCount != 1 ||
		telemetrySummary.ConnectedUsers.GatewayOnline != 1 ||
		len(telemetrySummary.TopPrivateApps) != 2 ||
		telemetrySummary.TopPrivateApps[0].App != "internal-git.svc.plus:443" ||
		len(telemetrySummary.TopUsers) != 1 ||
		telemetrySummary.TopUsers[0].DeviceID != "one-macbook" {
		t.Fatalf("unexpected AdminTelemetry summary: %#v", telemetrySummary)
	}
}

func TestUserGatewaysDirectEnrollAndNetworkModeUpdate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:user-enroll-mode-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-enroll", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	_, err = service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID: "net_uat", DisplayName: "UAT Network", CIDR: "10.77.0.0/29", GatewayID: "gw-uat-tw-xconnect",
			GatewayWireGuardKey: gwKey, GatewayWireGuardAddress: "10.77.0.1/32",
			GatewayEndpointHost: "tw-xconnect.svc.plus", GatewayEndpointPort: 51820,
			TransportServerName: "tw-xconnect.svc.plus", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "user-1",
		},
		Invite: BootstrapInvite{DeviceID: "gw-uat-tw-xconnect", Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}, "seed-token")
	if err != nil {
		t.Fatal(err)
	}

	gateways, err := service.UserGateways(t.Context(), "user-1")
	if err != nil || len(gateways) != 1 {
		t.Fatalf("user gateways=%#v err=%v", gateways, err)
	}
	if gateways[0].GatewayID != "gw-uat-tw-xconnect" || gateways[0].ConnectionMode != ConnectionModeWGVLESSL3 {
		t.Fatalf("unexpected gateway summary: %#v", gateways[0])
	}

	// Enrolling on another user's network must fail with ErrForbidden.
	oneKey1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32))
	if _, err := service.UserEnrollDevice(t.Context(), "other-user", UserEnrollRequest{
		NetworkID:          "net_uat",
		DeviceID:           "one-laptop",
		Platform:           "darwin",
		WireGuardPublicKey: oneKey1,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for foreign user enroll, got %v", err)
	}

	// Direct enrollment succeeds for owner.
	enrolled, err := service.UserEnrollDevice(t.Context(), "user-1", UserEnrollRequest{
		NetworkID:          "net_uat",
		GatewayID:          "gw-uat-tw-xconnect",
		DeviceID:           "one-laptop",
		Name:               "My MacBook",
		Platform:           "darwin",
		Hostname:           "macbook.local",
		WireGuardPublicKey: oneKey1,
	})
	if err != nil {
		t.Fatalf("user enroll device: %v", err)
	}
	if enrolled.Device.ID != "one-laptop" || enrolled.Device.WireGuardAddress != "10.77.0.2/32" || !strings.HasPrefix(enrolled.DeviceCredential.Credential, "xdc_") || !strings.HasPrefix(enrolled.EnrollmentToken, "xenr_") {
		t.Fatalf("unexpected enrollment response: %#v", enrolled)
	}

	// Re-enrolling the same device rotates its key & credential while preserving its IPv4 /32 address.
	oneKey2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32))
	reenrolled, err := service.UserEnrollDevice(t.Context(), "user-1", UserEnrollRequest{
		NetworkID:          "net_uat",
		GatewayID:          "gw-uat-tw-xconnect",
		DeviceID:           "one-laptop",
		Name:               "My MacBook",
		Platform:           "darwin",
		Hostname:           "macbook.local",
		WireGuardPublicKey: oneKey2,
	})
	if err != nil {
		t.Fatalf("user re-enroll device: %v", err)
	}
	if reenrolled.Device.WireGuardAddress != "10.77.0.2/32" || reenrolled.DeviceCredential.Credential == enrolled.DeviceCredential.Credential {
		t.Fatalf("expected address reuse and credential rotation, got %#v", reenrolled)
	}

	// Update network connection mode.
	if _, err := service.AdminUpdateNetworkMode(t.Context(), "user-1", "net_uat", "invalid_mode"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for invalid mode, got %v", err)
	}
	updatedNet, err := service.AdminUpdateNetworkMode(t.Context(), "user-1", "net_uat", ConnectionModeWGUDPL3)
	if err != nil {
		t.Fatalf("update network mode: %v", err)
	}
	if updatedNet.ConnectionMode != ConnectionModeWGUDPL3 || updatedNet.ConfigGeneration != 4 {
		t.Fatalf("unexpected updated network: %#v", updatedNet)
	}
}
