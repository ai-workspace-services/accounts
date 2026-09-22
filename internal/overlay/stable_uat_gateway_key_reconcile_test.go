package overlay

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

func TestAdminBootstrapReconcilesOnlyStableUATGatewayKey(t *testing.T) {
	service, db, now := newStableGatewayTestService(t)
	oldKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	newKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	bootstrap := BootstrapConfig{
		Network: BootstrapNetwork{
			ID: stableUATNetworkID, DisplayName: "UAT", CIDR: "10.77.0.0/24",
			GatewayID: stableUATGatewayID, GatewayWireGuardKey: oldKey,
			GatewayWireGuardAddress: "10.77.0.1/32", GatewayEndpointHost: stableUATGatewayHost,
			GatewayEndpointPort: 51820, TransportServerName: stableUATGatewayHost,
			TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "owner-uat",
		},
		Invite: BootstrapInvite{DeviceID: stableUATGatewayID, Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}
	// Complete the initial enrollment so the following bootstrap must reconcile
	// an existing active Gateway record rather than merely seed a network.
	firstToken := newOpaqueToken("xjt_")
	if _, err := service.AdminBootstrap(t.Context(), bootstrap, "https://accounts.example.test", firstToken); err != nil {
		t.Fatal(err)
	}
	initial := ExchangeRequest{JoinToken: firstToken, DeviceID: stableUATGatewayID, Name: "Gateway", Hostname: stableUATGatewayHost, Platform: "linux", Role: RoleGateway, WireGuardPublicKey: oldKey}
	if _, err := service.Exchange(t.Context(), initial); err != nil {
		t.Fatal(err)
	}

	bootstrap.Network.GatewayWireGuardKey = newKey
	rotationToken := newOpaqueToken("xjt_")
	if _, err := service.AdminBootstrap(t.Context(), bootstrap, "https://accounts.example.test", rotationToken); err != nil {
		t.Fatal(err)
	}
	rotated := initial
	rotated.JoinToken, rotated.WireGuardPublicKey = rotationToken, newKey
	if _, err := service.Exchange(t.Context(), rotated); err != nil {
		t.Fatalf("expected stable UAT Gateway key reconciliation: %v", err)
	}

	var device DeviceRecord
	if err := db.Where("id = ?", stableUATGatewayID).First(&device).Error; err != nil {
		t.Fatal(err)
	}
	if device.WireGuardPublicKey != newKey || device.Status != "active" {
		t.Fatalf("unexpected reconciled device: %+v", device)
	}
	var network NetworkRecord
	if err := db.Where("id = ?", stableUATNetworkID).First(&network).Error; err != nil {
		t.Fatal(err)
	}
	if network.ConfigGeneration < 2 {
		t.Fatalf("expected generation bump after Gateway rekey, got %d", network.ConfigGeneration)
	}
}
