package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/internal/agentserver"
	"account/internal/auth"
	"account/internal/overlay"
	"account/internal/store"
)

func TestOverlayGatewayDiscoveryEnrollAdminTelemetryAndModeRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:api-overlay-telemetry-enroll?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := overlay.NewService(db, overlay.Config{SigningKeyID: "zero-api-key", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore()
	user := &store.User{
		ID:            "11111111-1111-4111-8111-111111111111",
		Name:          "zero-owner",
		Email:         "zero-owner@example.test",
		Role:          store.RoleUser,
		Active:        true,
		EmailVerified: true,
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	sessionToken := "user-session-token-123"
	if err := st.CreateSession(context.Background(), sessionToken, user.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tokenService := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public-token",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: time.Hour,
		Store:         st,
	})
	pair, err := tokenService.GenerateTokenPair(user.ID, user.Email, []string{store.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	accessToken := pair.AccessToken

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	joinToken, err := service.Seed(context.Background(), overlay.BootstrapConfig{
		Network: overlay.BootstrapNetwork{
			ID: "net_uat", DisplayName: "UAT Network", CIDR: "10.77.0.0/24", GatewayID: "gw-uat-tw-xconnect",
			GatewayWireGuardKey: gwKey, GatewayWireGuardAddress: "10.77.0.1/32",
			GatewayEndpointHost: "tw-xconnect.svc.plus", GatewayEndpointPort: 51820,
			TransportServerName: "tw-xconnect.svc.plus", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: user.ID,
		},
		Invite: overlay.BootstrapInvite{DeviceID: "gw-uat-tw-xconnect", Platform: "linux", Role: overlay.RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Exchange(context.Background(), overlay.ExchangeRequest{
		JoinToken:          joinToken,
		DeviceID:           "gw-uat-tw-xconnect",
		Platform:           "linux",
		Role:               overlay.RoleGateway,
		WireGuardPublicKey: gwKey,
	}); err != nil {
		t.Fatal(err)
	}

	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: "gw-uat-tw-xconnect", NodeID: "gw-uat-tw-xconnect", Token: "agent-gw-token", Groups: []string{"resource:plus"}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithOverlayService(service), WithTokenService(tokenService), WithAgentRegistry(registry))

	// 1. User discovers gateways via GET /api/overlay/v1/gateways
	gwReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateways", nil)
	gwReq.Header.Set("Authorization", "Bearer "+accessToken)
	gwResp := httptest.NewRecorder()
	router.ServeHTTP(gwResp, gwReq)
	if gwResp.Code != http.StatusOK {
		t.Fatalf("GET /api/overlay/v1/gateways status=%d body=%s", gwResp.Code, gwResp.Body.String())
	}
	var gwPayload struct {
		Gateways []overlay.UserGatewaySummary `json:"gateways"`
	}
	if err := json.Unmarshal(gwResp.Body.Bytes(), &gwPayload); err != nil || len(gwPayload.Gateways) != 1 || gwPayload.Gateways[0].GatewayID != "gw-uat-tw-xconnect" {
		t.Fatalf("unexpected gateways payload: %#v err=%v", gwPayload, err)
	}

	// 2. User enrolls One device via POST /api/overlay/v1/devices/enroll
	oneKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	enrollBody, _ := json.Marshal(overlay.UserEnrollRequest{
		NetworkID:          "net_uat",
		GatewayID:          "gw-uat-tw-xconnect",
		DeviceID:           "one-macbook",
		Name:               "MacBook",
		Platform:           "darwin",
		Hostname:           "macbook.local",
		WireGuardPublicKey: oneKey,
	})
	enrollReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/devices/enroll", bytes.NewReader(enrollBody))
	enrollReq.Header.Set("Authorization", "Bearer "+accessToken)
	enrollReq.Header.Set("Content-Type", "application/json")
	enrollResp := httptest.NewRecorder()
	router.ServeHTTP(enrollResp, enrollReq)
	if enrollResp.Code != http.StatusOK {
		t.Fatalf("POST /api/overlay/v1/devices/enroll status=%d body=%s", enrollResp.Code, enrollResp.Body.String())
	}
	var exchange overlay.ExchangeResponse
	if err := json.Unmarshal(enrollResp.Body.Bytes(), &exchange); err != nil || exchange.Device.ID != "one-macbook" || exchange.Device.WireGuardAddress != "10.77.0.2/32" {
		t.Fatalf("unexpected enroll response: %#v err=%v", exchange, err)
	}

	// 3. Gateway reports telemetry via POST /api/agent-server/v1/status
	statusBody := `{
		"agentId": "gw-uat-tw-xconnect",
		"role": "gateway",
		"healthy": true,
		"xray": {
			"running": true,
			"nodeId": "gw-uat-tw-xconnect",
			"networkId": "net_uat"
		},
		"gatewayTelemetry": {
			"schema_version": 1,
			"network_id": "net_uat",
			"gateway_id": "gw-uat-tw-xconnect",
			"connection_mode": "wg_vless_l3",
			"applied_generation": 3,
			"wireguard_running": true,
			"xray_running": true,
			"relay_running": true,
			"traffic": {
				"rx_bytes_total": 50000,
				"tx_bytes_total": 90000,
				"internet_rx_bytes": 30000,
				"internet_tx_bytes": 60000,
				"private_rx_bytes": 20000,
				"private_tx_bytes": 30000
			},
			"peers": [
				{
					"device_id": "one-macbook",
					"wireguard_address": "10.77.0.2/32",
					"connected": true,
					"path": "direct",
					"rx_bytes": 20000,
					"tx_bytes": 30000,
					"rtt_ms": 22
				}
			],
			"top_apps": [
				{
					"app": "git.internal:443",
					"rx_bytes": 20000,
					"tx_bytes": 30000,
					"connections": 6
				}
			]
		}
	}`
	statusReq := httptest.NewRequest(http.MethodPost, "/api/agent-server/v1/status", bytes.NewBufferString(statusBody))
	statusReq.Header.Set("Authorization", "Bearer agent-gw-token")
	statusReq.Header.Set("X-Agent-ID", "gw-uat-tw-xconnect")
	statusReq.Header.Set("Content-Type", "application/json")
	statusResp := httptest.NewRecorder()
	router.ServeHTTP(statusResp, statusReq)
	if statusResp.Code != http.StatusNoContent {
		t.Fatalf("POST /api/agent-server/v1/status status=%d body=%s", statusResp.Code, statusResp.Body.String())
	}

	// 4. Admin fetches telemetry via GET /api/overlay/v1/admin/telemetry
	telReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/admin/telemetry", nil)
	telReq.Header.Set("Authorization", "Bearer "+accessToken)
	telReq.Header.Set("X-Session-Token", sessionToken)
	telResp := httptest.NewRecorder()
	router.ServeHTTP(telResp, telReq)
	if telResp.Code != http.StatusOK {
		t.Fatalf("GET /api/overlay/v1/admin/telemetry status=%d body=%s", telResp.Code, telResp.Body.String())
	}
	var adminTel overlay.AdminTelemetry
	if err := json.Unmarshal(telResp.Body.Bytes(), &adminTel); err != nil {
		t.Fatal(err)
	}
	if adminTel.Status != "available" || adminTel.SASETraffic.RxBytesTotal != 50000 || adminTel.ConnectedUsers.ConnectedCount != 1 || len(adminTel.TopPrivateApps) != 1 || len(adminTel.TopUsers) != 1 {
		t.Fatalf("unexpected admin telemetry response: %#v", adminTel)
	}

	// 5. Admin updates network mode via PUT /api/overlay/v1/admin/networks/:networkID/mode
	modeReq := httptest.NewRequest(http.MethodPut, "/api/overlay/v1/admin/networks/net_uat/mode", bytes.NewBufferString(`{"connection_mode":"wg_udp_l3"}`))
	modeReq.Header.Set("Authorization", "Bearer "+accessToken)
	modeReq.Header.Set("X-Session-Token", sessionToken)
	modeReq.Header.Set("Content-Type", "application/json")
	modeResp := httptest.NewRecorder()
	router.ServeHTTP(modeResp, modeReq)
	if modeResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/overlay/v1/admin/networks/net_uat/mode status=%d body=%s", modeResp.Code, modeResp.Body.String())
	}
	var updatedNet overlay.Network
	if err := json.Unmarshal(modeResp.Body.Bytes(), &updatedNet); err != nil || updatedNet.ConnectionMode != overlay.ConnectionModeWGUDPL3 {
		t.Fatalf("unexpected network mode update response: %#v err=%v", updatedNet, err)
	}
}
