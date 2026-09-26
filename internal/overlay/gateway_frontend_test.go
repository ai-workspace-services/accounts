package overlay

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

func frontendBootstrap(frontend, socket string) BootstrapConfig {
	return BootstrapConfig{
		Network: BootstrapNetwork{ID: "net_shared_vault", DisplayName: "Vault", CIDR: "10.79.0.0/24", GatewayID: "vault-prod-0", GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), GatewayWireGuardAddress: "10.79.0.1/32", GatewayEndpointHost: "vault-xconnect.svc.plus", GatewayEndpointPort: 51820, TransportServerName: "vault-xconnect.svc.plus", TransportPort: 443, TransportAuthID: "33333333-3333-3333-3333-333333333333", GatewayFrontend: frontend, GatewayListenSocket: socket},
		Invite:  BootstrapInvite{DeviceID: "vault-prod-1", Role: RoleOne, Platform: "linux", ExpiresAt: time.Now().UTC().Add(time.Hour)},
	}
}

func TestGatewayFrontendPrefersTheNetworkOverTheDeployment(t *testing.T) {
	t.Setenv("XCONNECT_GATEWAY_XRAY_FRONTEND", "")
	t.Setenv("XCONNECT_GATEWAY_XRAY_LISTEN_SOCKET", "")
	frontend, socket, err := gatewayFrontend(NetworkRecord{GatewayFrontend: GatewayFrontendCaddyUnixH2C})
	if err != nil || frontend != GatewayFrontendCaddyUnixH2C || socket != DefaultGatewayListenSocket {
		t.Fatalf("network frontend ignored: %q %q %v", frontend, socket, err)
	}
	frontend, socket, err = gatewayFrontend(NetworkRecord{})
	if err != nil || frontend != GatewayFrontendDirectTLS || socket != "" {
		t.Fatalf("unset network must keep the deployment default: %q %q %v", frontend, socket, err)
	}
	t.Setenv("XCONNECT_GATEWAY_XRAY_FRONTEND", GatewayFrontendCaddyUnixH2C)
	frontend, _, err = gatewayFrontend(NetworkRecord{GatewayFrontend: GatewayFrontendDirectTLS})
	if err != nil || frontend != GatewayFrontendDirectTLS {
		t.Fatalf("a direct-TLS network must not inherit a Caddy deployment: %q %v", frontend, err)
	}
}

func TestBootstrapRejectsInvalidGatewayFrontend(t *testing.T) {
	for _, tc := range []struct{ frontend, socket string }{
		{"nginx", ""},
		{GatewayFrontendCaddyUnixH2C, "relative.sock"},
		{"", "/run/xconnect-gateway/xray.sock"},
	} {
		if err := frontendBootstrap(tc.frontend, tc.socket).Network.validate(); err != ErrInvalidInput {
			t.Fatalf("frontend=%q socket=%q: want ErrInvalidInput, got %v", tc.frontend, tc.socket, err)
		}
	}
	if err := frontendBootstrap(GatewayFrontendCaddyUnixH2C, "").Network.validate(); err != nil {
		t.Fatalf("Caddy frontend with the default socket must be valid: %v", err)
	}
}

func TestReseedingANewGatewayFrontendIssuesANewGeneration(t *testing.T) {
	service, _, _ := newOverlayHTTPTest(t)
	generation := func() (NetworkRecord, uint64) {
		t.Helper()
		var record NetworkRecord
		if err := service.repo.DB.Where("id = ?", "net_shared_vault").First(&record).Error; err != nil {
			t.Fatal(err)
		}
		return record, record.ConfigGeneration
	}
	if _, err := service.Seed(t.Context(), frontendBootstrap("", ""), ""); err != nil {
		t.Fatal(err)
	}
	_, before := generation()
	if _, err := service.Seed(t.Context(), frontendBootstrap("", ""), ""); err != nil {
		t.Fatal(err)
	}
	if _, same := generation(); same != before {
		t.Fatalf("an unchanged frontend must keep generation %d, got %d", before, same)
	}
	if _, err := service.Seed(t.Context(), frontendBootstrap(GatewayFrontendCaddyUnixH2C, DefaultGatewayListenSocket), ""); err != nil {
		t.Fatal(err)
	}
	record, after := generation()
	if after != before+1 || record.GatewayFrontend != GatewayFrontendCaddyUnixH2C || record.GatewayListenSocket != DefaultGatewayListenSocket {
		t.Fatalf("frontend change must be stored with a new generation: gen %d->%d record=%+v", before, after, record)
	}
}
