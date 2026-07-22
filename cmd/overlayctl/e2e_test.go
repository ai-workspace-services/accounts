package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"account/api"
	"account/internal/store"
)

func TestOverlayCLIEndToEndAgainstLocalAccountsServer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		email    = "overlay-e2e@example.com"
		password = "supersecure"
		deviceID = "e2e-macos"
	)

	st := store.NewMemoryStore()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user := &store.User{
		Name:          "Overlay E2E",
		Email:         email,
		PasswordHash:  string(hash),
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	router := gin.New()
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-token")
	api.RegisterRoutes(router, api.WithStore(st), api.WithEmailVerification(false))
	server := httptest.NewServer(router)
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)

	postOverlayNodeHeartbeatForTest(t, server.URL, "internal-token")
	privateKey, publicKey, err := generateWireGuardKeypair()
	if err != nil {
		t.Fatalf("generate wireguard keypair: %v", err)
	}

	executeOverlayCommand(t, "login", "--server", server.URL, "--email", email, "--password", password)
	executeOverlayCommand(t, "register-device",
		"--device-id", deviceID,
		"--public-key", publicKey,
		"--private-key", privateKey,
	)
	executeOverlayCommand(t, "sync-config")
	executeOverlayCommand(t, "render")
	executeOverlayCommand(t, "preflight")
	executeOverlayCommand(t, "ack-config")

	statePath := filepath.Join(home, ".xoverlay", "session.json")
	configPath := filepath.Join(home, ".xoverlay", "overlay-config.json")
	wgPath := filepath.Join(home, ".xoverlay", "xwg0.conf")
	xrayPath := filepath.Join(home, ".xoverlay", "xray-overlay.json")

	for _, path := range []string{statePath, configPath, wgPath, xrayPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist: %v", path, err)
		}
	}

	var state stateFile
	readJSONForTest(t, statePath, &state)
	if state.DeviceID != deviceID {
		t.Fatalf("expected device id %q in state, got %q", deviceID, state.DeviceID)
	}
	if state.WireGuardPublicKey != publicKey {
		t.Fatalf("expected public key to be persisted, got %q", state.WireGuardPublicKey)
	}

	var config overlayConfig
	readJSONForTest(t, configPath, &config)
	if config.Device.ID != deviceID {
		t.Fatalf("expected config device id %q, got %q", deviceID, config.Device.ID)
	}
	if config.WireGuard.PeerEndpoint != "127.0.0.1:51830" {
		t.Fatalf("expected local transport endpoint, got %q", config.WireGuard.PeerEndpoint)
	}
	if strings.TrimSpace(config.Transport.UUID) == "" {
		t.Fatalf("expected transport UUID to be populated")
	}
	if config.Transport.UUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("expected transport UUID from internal heartbeat, got %q", config.Transport.UUID)
	}
	if config.Transport.Server != "xworkmate-bridge.svc.plus" {
		t.Fatalf("expected gateway server from internal heartbeat, got %q", config.Transport.Server)
	}
	if config.WireGuard.PeerPublicKey != "1staGq8lmHFRFRFNj2QOFx/MPxb/1fFV4tawC6xSi1Q=" {
		t.Fatalf("expected gateway public key from internal heartbeat, got %q", config.WireGuard.PeerPublicKey)
	}
	if config.WireGuard.GatewayWireGuardIP != "172.29.10.1" {
		t.Fatalf("expected gateway WireGuard IP from internal heartbeat, got %q", config.WireGuard.GatewayWireGuardIP)
	}

	wgBytes, err := os.ReadFile(wgPath)
	if err != nil {
		t.Fatalf("read wireguard config: %v", err)
	}
	if !strings.Contains(string(wgBytes), "PrivateKey = "+privateKey) {
		t.Fatalf("expected rendered WireGuard config to contain private key, got:\n%s", string(wgBytes))
	}

	devices, err := st.ListOverlayDevicesByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("list overlay devices: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != deviceID {
		t.Fatalf("expected one registered device %q, got %#v", deviceID, devices)
	}
}

func postOverlayNodeHeartbeatForTest(t *testing.T, serverURL, token string) {
	t.Helper()
	body := bytes.NewBufferString(`{
		"node_id": "xworkmate-bridge",
		"network_id": "xworkmate-private",
		"name": "XWorkmate Bridge",
		"role": "gateway",
		"region": "jp",
		"wireguard_public_key": "1staGq8lmHFRFRFNj2QOFx/MPxb/1fFV4tawC6xSi1Q=",
		"wireguard_address": "172.29.10.1",
		"endpoint_host": "xworkmate-bridge.svc.plus",
		"endpoint_port": 2443,
		"transport_type": "vless-tls",
		"transport_security": "tls",
		"transport_uuid": "11111111-1111-1111-1111-111111111111"
	}`)
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/internal/overlay/nodes/heartbeat", body)
	if err != nil {
		t.Fatalf("build overlay node heartbeat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post overlay node heartbeat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overlay node heartbeat failed: %s", resp.Status)
	}
}

func executeOverlayCommand(t *testing.T, args ...string) {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("overlayctl %s failed: %v", strings.Join(args, " "), err)
	}
}

func readJSONForTest(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
