package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOverlayConfig() overlayConfig {
	var config overlayConfig
	config.SchemaVersion = 1
	config.Revision = "123"
	config.Digest = "digest"
	config.Network.ID = "xworkmate-private"
	config.Device.ID = "shenlan-macos"
	config.WireGuard.Interface = "xwg0"
	config.WireGuard.Address = "172.29.10.123/32"
	config.WireGuard.MTU = 1280
	config.WireGuard.DNS = []string{"172.29.10.1"}
	config.WireGuard.PeerPublicKey = "1staGq8lmHFRFRFNj2QOFx/MPxb/1fFV4tawC6xSi1Q="
	config.WireGuard.PeerAllowedIPs = []string{"172.29.10.0/24"}
	config.WireGuard.PeerEndpoint = "127.0.0.1:51830"
	config.WireGuard.PersistentKeepalive = 25
	config.WireGuard.GatewayWireGuardIP = "172.29.10.1"
	config.Transport.Server = "xworkmate-bridge.svc.plus"
	config.Transport.Port = 2443
	config.Transport.Type = "vless-tls"
	config.Transport.UUID = "11111111-1111-1111-1111-111111111111"
	config.Transport.Security = "tls"
	config.Transport.PacketEncoding = "xudp"
	config.Transport.LocalPort = 51830
	return config
}

func TestRenderPlaybooksClientFragment(t *testing.T) {
	state := stateFile{
		DeviceID:           "shenlan-macos",
		WireGuardPublicKey: "jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
	}

	rendered, err := renderPlaybooksClientFragment(state, testOverlayConfig(), []string{
		"jp-xhttp-contabo.svc.plus",
		"cn-xworkmate-bridge.svc.plus",
	})
	if err != nil {
		t.Fatalf("render playbooks fragment: %v", err)
	}

	for _, want := range []string{
		"xworkmate_bridge_distributed_vpn_clients:",
		"id: shenlan-macos",
		"wg_ip: 172.29.10.123",
		"public_key: jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
		"attach_to:",
		"- jp-xhttp-contabo.svc.plus",
		"- cn-xworkmate-bridge.svc.plus",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected rendered fragment to contain %q, got:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "/32") {
		t.Fatalf("playbooks wg_ip must not include CIDR suffix, got:\n%s", rendered)
	}
}

func TestMergePlaybooksClientFileReplacesExistingDevice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xworkmate_bridge_distributed.yml")
	initial := `---
xworkmate_bridge_distributed_topology: dual-node
xworkmate_bridge_distributed_vpn_clients:
  - id: shenlan-macos
    wg_ip: 172.29.10.10
    public_key: iYlnFaWiMfMelpiN8ZV2SwCDrLihqtJXvHUsM3BN9zU=
    attach_to:
      - jp-xhttp-contabo.svc.plus
  - id: shenlan-ios
    wg_ip: 172.29.10.11
    public_key: I/zCL7gLWrY6FZiLXUs7i/vivU5Xuo8r7EbkNhtv12w=
    attach_to:
      - cn-xworkmate-bridge.svc.plus
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial group vars: %v", err)
	}

	err := mergePlaybooksClientFile(path, playbooksClient{
		ID:        "shenlan-macos",
		WGIP:      "172.29.10.123",
		PublicKey: "jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
	})
	if err != nil {
		t.Fatalf("merge playbooks client: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read merged group vars: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{
		"xworkmate_bridge_distributed_topology: dual-node",
		"id: shenlan-macos",
		"wg_ip: 172.29.10.123",
		"public_key: jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
		"- jp-xhttp-contabo.svc.plus",
		"id: shenlan-ios",
		"public_key: I/zCL7gLWrY6FZiLXUs7i/vivU5Xuo8r7EbkNhtv12w=",
		"- cn-xworkmate-bridge.svc.plus",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected merged group vars to contain %q, got:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "iYlnFaWiMfMelpiN8ZV2SwCDrLihqtJXvHUsM3BN9zU=") {
		t.Fatalf("expected old key to be replaced, got:\n%s", rendered)
	}
	if count := strings.Count(rendered, "id: shenlan-macos"); count != 1 {
		t.Fatalf("expected shenlan-macos once, got %d:\n%s", count, rendered)
	}
}

func TestMergePlaybooksClientFileReplacesAttachToWhenProvided(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xworkmate_bridge_distributed.yml")
	initial := `---
xworkmate_bridge_distributed_vpn_clients:
  - id: shenlan-macos
    wg_ip: 172.29.10.10
    public_key: iYlnFaWiMfMelpiN8ZV2SwCDrLihqtJXvHUsM3BN9zU=
    attach_to:
      - cn-xworkmate-bridge.svc.plus
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial group vars: %v", err)
	}

	err := mergePlaybooksClientFile(path, playbooksClient{
		ID:        "shenlan-macos",
		WGIP:      "172.29.10.123",
		PublicKey: "jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
		AttachTo:  []string{"jp-xhttp-contabo.svc.plus"},
	})
	if err != nil {
		t.Fatalf("merge playbooks client: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read merged group vars: %v", err)
	}
	rendered := string(data)
	if !strings.Contains(rendered, "- jp-xhttp-contabo.svc.plus") {
		t.Fatalf("expected new attach_to to be written, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "- cn-xworkmate-bridge.svc.plus") {
		t.Fatalf("expected old attach_to to be replaced, got:\n%s", rendered)
	}
}

func TestBuildConfigAckPayload(t *testing.T) {
	state := stateFile{
		DeviceID:  "state-device",
		NetworkID: "state-network",
	}
	config := testOverlayConfig()
	appliedAt := time.Date(2026, 6, 1, 10, 20, 30, 0, time.UTC)

	payload, err := buildConfigAckPayload(state, config, appliedAt)
	if err != nil {
		t.Fatalf("build ack payload: %v", err)
	}

	expected := map[string]string{
		"device_id":  "shenlan-macos",
		"network_id": "xworkmate-private",
		"revision":   "123",
		"digest":     "digest",
		"applied_at": "2026-06-01T10:20:30Z",
	}
	for key, want := range expected {
		if got := payload[key]; got != want {
			t.Fatalf("expected %s=%q, got %q", key, want, got)
		}
	}
}

func TestRenderWireGuardConfigUsesLocalTransportEndpoint(t *testing.T) {
	rendered := renderWireGuardConfig(testOverlayConfig(), "private-key")

	for _, want := range []string{
		"PrivateKey = private-key",
		"Address = 172.29.10.123/32",
		"PublicKey = 1staGq8lmHFRFRFNj2QOFx/MPxb/1fFV4tawC6xSi1Q=",
		"AllowedIPs = 172.29.10.0/24",
		"Endpoint = 127.0.0.1:51830",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected WireGuard config to contain %q, got:\n%s", want, rendered)
		}
	}
}

func TestValidateOverlayRuntimeConfigRejectsMissingRuntimeFields(t *testing.T) {
	config := testOverlayConfig()
	config.Transport.UUID = ""
	config.WireGuard.GatewayWireGuardIP = ""

	err := validateOverlayRuntimeConfig(config)
	if err == nil {
		t.Fatalf("expected incomplete config error")
	}
	for _, want := range []string{"transport.uuid", "wireguard.gateway_wireguard_ip"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %q, got %v", want, err)
		}
	}
}

func TestValidateOverlayRuntimeConfigAcceptsCompleteConfig(t *testing.T) {
	if err := validateOverlayRuntimeConfig(testOverlayConfig()); err != nil {
		t.Fatalf("expected complete config to validate: %v", err)
	}
}

func TestValidateOverlayRuntimeConfigRejectsInvalidTransportUUID(t *testing.T) {
	config := testOverlayConfig()
	config.Transport.UUID = "not-a-uuid"

	err := validateOverlayRuntimeConfig(config)
	if err == nil {
		t.Fatalf("expected invalid transport uuid error")
	}
	if !strings.Contains(err.Error(), "transport.uuid") {
		t.Fatalf("expected transport.uuid error, got %v", err)
	}
}

func TestValidateOverlayRuntimeConfigRejectsInvalidTransportFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*overlayConfig)
		want   string
	}{
		{
			name: "transport port too high",
			mutate: func(config *overlayConfig) {
				config.Transport.Port = 70000
			},
			want: "transport.port",
		},
		{
			name: "unsupported transport type",
			mutate: func(config *overlayConfig) {
				config.Transport.Type = "vless-reality"
			},
			want: "transport.type",
		},
		{
			name: "unsupported transport security",
			mutate: func(config *overlayConfig) {
				config.Transport.Security = "reality"
			},
			want: "transport.security",
		},
		{
			name: "invalid local port",
			mutate: func(config *overlayConfig) {
				config.Transport.LocalPort = 70000
			},
			want: "transport.local_port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testOverlayConfig()
			tt.mutate(&config)

			err := validateOverlayRuntimeConfig(config)
			if err == nil {
				t.Fatalf("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error to mention %q, got %v", tt.want, err)
			}
		})
	}
}

func TestDefaultConnectivityURL(t *testing.T) {
	if got := defaultConnectivityURL(testOverlayConfig()); got != "http://172.29.10.1:8787/api/ping" {
		t.Fatalf("unexpected connectivity URL: %q", got)
	}

	config := testOverlayConfig()
	config.WireGuard.GatewayWireGuardIP = ""
	if got := defaultConnectivityURL(config); got != "" {
		t.Fatalf("expected empty URL when gateway IP is missing, got %q", got)
	}
}

func TestReadPIDFileRejectsInvalidPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xray-overlay.pid")
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	if _, err := readPIDFile(path); err == nil {
		t.Fatalf("expected invalid pid error")
	}
}

func TestClearStalePIDFileRemovesInvalidPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xray-overlay.pid")
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if err := clearStalePIDFile(path); err != nil {
		t.Fatalf("clear stale pid file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected invalid pid file to be removed, stat err=%v", err)
	}
}

func TestClearStalePIDFileRejectsRunningPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xray-overlay.pid")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write running pid file: %v", err)
	}
	if err := clearStalePIDFile(path); err == nil {
		t.Fatalf("expected running pid file to be rejected")
	}
}

func TestRenderXrayConfigTargetsGatewayWireGuardUDP(t *testing.T) {
	rendered := renderXrayConfig(testOverlayConfig())

	for _, want := range []string{
		`"protocol": "dokodemo-door"`,
		`"address": "172.29.10.1"`,
		`"port": 51820`,
		`"address": "xworkmate-bridge.svc.plus"`,
		`"id": "11111111-1111-1111-1111-111111111111"`,
		`"packetEncoding": "xudp"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected Xray config to contain %q, got:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, `"flow"`) {
		t.Fatalf("default Xray config must not include VLESS flow, got:\n%s", rendered)
	}
}

func TestStripCIDRSuffix(t *testing.T) {
	if got := stripCIDRSuffix("172.29.10.123/32"); got != "172.29.10.123" {
		t.Fatalf("expected CIDR suffix stripped, got %q", got)
	}
	if got := stripCIDRSuffix("172.29.10.123"); got != "172.29.10.123" {
		t.Fatalf("expected plain IP preserved, got %q", got)
	}
}
