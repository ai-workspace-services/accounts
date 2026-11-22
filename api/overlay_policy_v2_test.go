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
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/internal/auth"
	"account/internal/overlay"
	"account/internal/store"
)

func TestOverlayPolicyUpdateAuditExplainAndGatewayV2Routes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:api-overlay-policy-v2?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := overlay.NewService(db, overlay.Config{
		SigningKeyID:      "zero-policy-api-key",
		SigningPrivateKey: signer,
		Clock:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	st := store.NewMemoryStore()
	owner := &store.User{
		ID:            "11111111-1111-4111-8111-111111111111",
		Name:          "policy-owner",
		Email:         "policy-owner@example.test",
		Role:          store.RoleUser,
		Active:        true,
		EmailVerified: true,
	}
	if err := st.CreateUser(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	other := &store.User{
		ID:            "22222222-2222-4222-8222-222222222222",
		Name:          "other-user",
		Email:         "other-user@example.test",
		Role:          store.RoleUser,
		Active:        true,
		EmailVerified: true,
	}
	if err := st.CreateUser(context.Background(), other); err != nil {
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
	ownerTokens, err := tokenService.GenerateTokenPair(owner.ID, owner.Email, []string{store.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	otherTokens, err := tokenService.GenerateTokenPair(other.ID, other.Email, []string{store.RoleUser})
	if err != nil {
		t.Fatal(err)
	}

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	gwJoin, err := service.Seed(context.Background(), overlay.BootstrapConfig{
		Network: overlay.BootstrapNetwork{
			ID: "net_policy_v2", DisplayName: "Policy V2 Net", CIDR: "10.77.0.0/24", GatewayID: "gw-policy-1",
			GatewayWireGuardKey: gwKey, GatewayWireGuardAddress: "10.77.0.1/32",
			GatewayEndpointHost: "gw.policy.test", GatewayEndpointPort: 51820,
			TransportServerName: "gw.policy.test", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: owner.ID,
		},
		Invite: overlay.BootstrapInvite{DeviceID: "gw-policy-1", Platform: "linux", Role: overlay.RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	gwExchange, err := service.Exchange(context.Background(), overlay.ExchangeRequest{
		JoinToken:          gwJoin,
		DeviceID:           "gw-policy-1",
		Platform:           "linux",
		Role:               overlay.RoleGateway,
		WireGuardPublicKey: gwKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UserEnrollDevice(context.Background(), owner.ID, overlay.UserEnrollRequest{
		NetworkID:          "net_policy_v2",
		DeviceID:           "mac",
		Platform:           "darwin",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UserEnrollDevice(context.Background(), owner.ID, overlay.UserEnrollRequest{
		NetworkID:          "net_policy_v2",
		DeviceID:           "bot",
		Platform:           "linux",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	}); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithOverlayService(service), WithTokenService(tokenService))

	// 1. Unknown field rejected strictly on PUT /policy.
	badPutReq := httptest.NewRequest(http.MethodPut, "/api/overlay/v1/admin/networks/net_policy_v2/policy", bytes.NewBufferString(`{"default_action":"deny","unknown_field":true}`))
	badPutReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	badPutReq.Header.Set("Content-Type", "application/json")
	badPutResp := httptest.NewRecorder()
	router.ServeHTTP(badPutResp, badPutReq)
	if badPutResp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field on PUT policy, got %d: %s", badPutResp.Code, badPutResp.Body.String())
	}

	// 2. Valid PUT /policy updates policy and writes audit log.
	putBody := `{
		"default_action": "deny",
		"rules": [
			{
				"id": "allow-mac-bot",
				"action": "allow",
				"source_devices": ["mac"],
				"destination_devices": ["bot"],
				"protocols": ["tcp"],
				"ports": [443]
			}
		]
	}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/overlay/v1/admin/networks/net_policy_v2/policy", bytes.NewBufferString(putBody))
	putReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	putReq.Header.Set("Content-Type", "application/json")
	putResp := httptest.NewRecorder()
	router.ServeHTTP(putResp, putReq)
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT policy status=%d body=%s", putResp.Code, putResp.Body.String())
	}
	var updatedPolicy overlay.PolicyArtifact
	if err := json.Unmarshal(putResp.Body.Bytes(), &updatedPolicy); err != nil {
		t.Fatal(err)
	}
	if updatedPolicy.NetworkID != "net_policy_v2" || len(updatedPolicy.Rules) != 1 || updatedPolicy.Rules[0].ID != "allow-mac-bot" {
		t.Fatalf("unexpected PUT policy response: %#v", updatedPolicy)
	}

	auditEntries, err := st.ListAuditLogs(context.Background(), store.AuditLogFilter{ActionPrefix: store.AuditActionOverlayPolicyUpdate})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditEntries) != 1 {
		t.Fatalf("expected 1 policy update audit entry, got %d", len(auditEntries))
	}
	if auditEntries[0].ActorUUID != owner.ID || auditEntries[0].Details["network_id"] != "net_policy_v2" {
		t.Fatalf("unexpected audit entry: %#v", auditEntries[0])
	}
	if auditEntries[0].Details["default_action"] != "deny" {
		t.Fatalf("unexpected audit default_action: %#v", auditEntries[0].Details)
	}

	// 3. POST /policy/explain works for owner and isolates non-owner.
	explainBody := `{"source_device_id":"mac","destination_device_id":"bot","protocol":"tcp","port":443}`
	explainReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/admin/networks/net_policy_v2/policy/explain", bytes.NewBufferString(explainBody))
	explainReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	explainReq.Header.Set("Content-Type", "application/json")
	explainResp := httptest.NewRecorder()
	router.ServeHTTP(explainResp, explainReq)
	if explainResp.Code != http.StatusOK {
		t.Fatalf("POST explain status=%d body=%s", explainResp.Code, explainResp.Body.String())
	}
	var explained overlay.PolicyExplainResponse
	if err := json.Unmarshal(explainResp.Body.Bytes(), &explained); err != nil {
		t.Fatal(err)
	}
	if explained.Decision != "allow" || explained.MatchedRuleID != "allow-mac-bot" || explained.MatchedRuleIndex == nil || *explained.MatchedRuleIndex != 0 || explained.Reason != "matched_rule" || explained.SourceAddress != "10.77.0.2/32" || explained.DestinationAddress != "10.77.0.3/32" {
		t.Fatalf("unexpected explain response: %#v", explained)
	}

	otherExplainReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/admin/networks/net_policy_v2/policy/explain", bytes.NewBufferString(explainBody))
	otherExplainReq.Header.Set("Authorization", "Bearer "+otherTokens.AccessToken)
	otherExplainReq.Header.Set("Content-Type", "application/json")
	otherExplainResp := httptest.NewRecorder()
	router.ServeHTTP(otherExplainResp, otherExplainReq)
	if otherExplainResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-owner explain, got %d: %s", otherExplainResp.Code, otherExplainResp.Body.String())
	}

	badExplainReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/admin/networks/net_policy_v2/policy/explain", bytes.NewBufferString(`{"source_device_id":"mac","destination_device_id":"bot","protocol":"icmp","port":443}`))
	badExplainReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	badExplainReq.Header.Set("Content-Type", "application/json")
	badExplainResp := httptest.NewRecorder()
	router.ServeHTTP(badExplainResp, badExplainReq)
	if badExplainResp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for icmp with port on explain, got %d: %s", badExplainResp.Code, badExplainResp.Body.String())
	}

	// 4. Gateway signed-config v1 vs v2 negotiation via /api/overlay/v1/gateway/signed-config.
	gwV1Req := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateway/signed-config", nil)
	gwV1Req.Header.Set("Authorization", "Bearer "+gwExchange.EnrollmentToken)
	gwV1Req.Header.Set("Accept", "application/json")
	gwV1Resp := httptest.NewRecorder()
	router.ServeHTTP(gwV1Resp, gwV1Req)
	if gwV1Resp.Code != http.StatusOK || gwV1Resp.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("gateway v1 status=%d headers=%v body=%s", gwV1Resp.Code, gwV1Resp.Header(), gwV1Resp.Body.String())
	}
	if strings.Contains(gwV1Resp.Body.String(), `"policy"`) {
		t.Fatalf("v1 response unexpectedly contained policy: %s", gwV1Resp.Body.String())
	}

	gwV2Req := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateway/signed-config", nil)
	gwV2Req.Header.Set("Authorization", "Bearer "+gwExchange.EnrollmentToken)
	gwV2Req.Header.Set("Accept", overlay.GatewaySignedConfigV2MediaType)
	gwV2Resp := httptest.NewRecorder()
	router.ServeHTTP(gwV2Resp, gwV2Req)
	if gwV2Resp.Code != http.StatusOK || gwV2Resp.Header().Get("Content-Type") != overlay.GatewaySignedConfigV2MediaType || gwV2Resp.Header().Get("Vary") != "Accept" {
		t.Fatalf("gateway v2 status=%d headers=%v body=%s", gwV2Resp.Code, gwV2Resp.Header(), gwV2Resp.Body.String())
	}
	var gwV2 overlay.GatewaySignedConfig
	if err := json.Unmarshal(gwV2Resp.Body.Bytes(), &gwV2); err != nil || gwV2.SchemaVersion != 2 || gwV2.Policy == nil || len(gwV2.Policy.Rules) != 1 {
		t.Fatalf("unexpected gateway v2 payload: err=%v cfg=%#v", err, gwV2)
	}
}
