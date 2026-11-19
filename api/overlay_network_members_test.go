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

	"account/internal/auth"
	"account/internal/overlay"
	"account/internal/store"
)

func TestOverlayAdminNetworkMembersBindingRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:api-overlay-network-members?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := overlay.NewService(db, overlay.Config{SigningKeyID: "zero-members-api-key", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore()
	owner := &store.User{
		ID:            "11111111-1111-4111-8111-111111111111",
		Name:          "zero-owner",
		Email:         "zero-owner@example.test",
		Role:          store.RoleUser,
		Active:        true,
		EmailVerified: true,
	}
	member := &store.User{
		ID:            "22222222-2222-4222-8222-222222222222",
		Name:          "qq-member",
		Email:         "156405189@qq.com",
		Role:          store.RoleUser,
		Active:        true,
		EmailVerified: true,
	}
	foreign := &store.User{
		ID:            "33333333-3333-4333-8333-333333333333",
		Name:          "foreign-user",
		Email:         "foreign@example.test",
		Role:          store.RoleUser,
		Active:        true,
		EmailVerified: true,
	}
	for _, u := range []*store.User{owner, member, foreign} {
		if err := st.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
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
	memberTokens, err := tokenService.GenerateTokenPair(member.ID, member.Email, []string{store.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	foreignTokens, err := tokenService.GenerateTokenPair(foreign.ID, foreign.Email, []string{store.RoleUser})
	if err != nil {
		t.Fatal(err)
	}

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if _, err := service.Seed(context.Background(), overlay.BootstrapConfig{
		Network: overlay.BootstrapNetwork{
			ID: "net-ph-xconnect", DisplayName: "PH XConnect", CIDR: "10.88.0.0/24", GatewayID: "ph-xconnect-gateway-01",
			GatewayWireGuardKey: gwKey, GatewayWireGuardAddress: "10.88.0.1/32",
			GatewayEndpointHost: "ph.svc.plus", GatewayEndpointPort: 51820,
			TransportServerName: "ph.svc.plus", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: owner.ID,
		},
		Invite: overlay.BootstrapInvite{DeviceID: "ph-xconnect-gateway-01", Platform: "linux", Role: overlay.RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}, ""); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithOverlayService(service), WithTokenService(tokenService))

	// 1. Before binding, member sees no gateways and cannot enroll.
	gwReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateways", nil)
	gwReq.Header.Set("Authorization", "Bearer "+memberTokens.AccessToken)
	gwResp := httptest.NewRecorder()
	router.ServeHTTP(gwResp, gwReq)
	if gwResp.Code != http.StatusOK {
		t.Fatalf("GET /api/overlay/v1/gateways status=%d body=%s", gwResp.Code, gwResp.Body.String())
	}
	var gwPayload struct {
		Gateways []overlay.UserGatewaySummary `json:"gateways"`
	}
	if err := json.Unmarshal(gwResp.Body.Bytes(), &gwPayload); err != nil || len(gwPayload.Gateways) != 0 {
		t.Fatalf("expected 0 gateways before binding, got %#v err=%v", gwPayload, err)
	}

	oneKey1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	enrollBody1, _ := json.Marshal(overlay.UserEnrollRequest{
		NetworkID: "net-ph-xconnect", GatewayID: "ph-xconnect-gateway-01", DeviceID: "one-member-1",
		Platform: "darwin", WireGuardPublicKey: oneKey1,
	})
	enrollReq1 := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/devices/enroll", bytes.NewReader(enrollBody1))
	enrollReq1.Header.Set("Authorization", "Bearer "+memberTokens.AccessToken)
	enrollReq1.Header.Set("Content-Type", "application/json")
	enrollResp1 := httptest.NewRecorder()
	router.ServeHTTP(enrollResp1, enrollReq1)
	if enrollResp1.Code != http.StatusForbidden {
		t.Fatalf("expected 403 before binding, got %d body=%s", enrollResp1.Code, enrollResp1.Body.String())
	}

	// 2. Foreign user cannot manage another owner's network members.
	foreignBindReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/admin/networks/net-ph-xconnect/members", bytes.NewBufferString(`{"email":"156405189@qq.com"}`))
	foreignBindReq.Header.Set("Authorization", "Bearer "+foreignTokens.AccessToken)
	foreignBindReq.Header.Set("Content-Type", "application/json")
	foreignBindResp := httptest.NewRecorder()
	router.ServeHTTP(foreignBindResp, foreignBindReq)
	if foreignBindResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign bind, got %d body=%s", foreignBindResp.Code, foreignBindResp.Body.String())
	}

	// 3. Binding nonexistent email returns 404 user_not_found.
	missingReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/admin/networks/net-ph-xconnect/members", bytes.NewBufferString(`{"email":"missing@example.test"}`))
	missingReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	missingReq.Header.Set("Content-Type", "application/json")
	missingResp := httptest.NewRecorder()
	router.ServeHTTP(missingResp, missingReq)
	if missingResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 user_not_found, got %d body=%s", missingResp.Code, missingResp.Body.String())
	}
	var errPayload map[string]any
	if err := json.Unmarshal(missingResp.Body.Bytes(), &errPayload); err != nil || errPayload["error"] != "user_not_found" {
		t.Fatalf("expected error=user_not_found, got %#v", errPayload)
	}

	// 4. Owner binds member by email.
	bindReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/admin/networks/net-ph-xconnect/members", bytes.NewBufferString(`{"email":"156405189@QQ.COM"}`))
	bindReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	bindReq.Header.Set("Content-Type", "application/json")
	bindResp := httptest.NewRecorder()
	router.ServeHTTP(bindResp, bindReq)
	if bindResp.Code != http.StatusOK {
		t.Fatalf("POST members status=%d body=%s", bindResp.Code, bindResp.Body.String())
	}
	var bindPayload struct {
		Member overlay.NetworkMemberSummary `json:"member"`
	}
	if err := json.Unmarshal(bindResp.Body.Bytes(), &bindPayload); err != nil || bindPayload.Member.UserID != member.ID || bindPayload.Member.Email != "156405189@qq.com" {
		t.Fatalf("unexpected bind payload: %#v err=%v", bindPayload, err)
	}

	// 5. Owner lists members.
	listReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/admin/networks/net-ph-xconnect/members", nil)
	listReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	listResp := httptest.NewRecorder()
	router.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("GET members status=%d body=%s", listResp.Code, listResp.Body.String())
	}
	var listPayload struct {
		Members []overlay.NetworkMemberSummary `json:"members"`
	}
	if err := json.Unmarshal(listResp.Body.Bytes(), &listPayload); err != nil || len(listPayload.Members) != 1 || listPayload.Members[0].UserID != member.ID {
		t.Fatalf("unexpected list payload: %#v err=%v", listPayload, err)
	}

	// 6. Member discovers gateway and enrolls device.
	gwReq2 := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateways", nil)
	gwReq2.Header.Set("Authorization", "Bearer "+memberTokens.AccessToken)
	gwResp2 := httptest.NewRecorder()
	router.ServeHTTP(gwResp2, gwReq2)
	if gwResp2.Code != http.StatusOK {
		t.Fatalf("GET gateways after bind status=%d body=%s", gwResp2.Code, gwResp2.Body.String())
	}
	var gwPayload2 struct {
		Gateways []overlay.UserGatewaySummary `json:"gateways"`
	}
	if err := json.Unmarshal(gwResp2.Body.Bytes(), &gwPayload2); err != nil || len(gwPayload2.Gateways) != 1 || gwPayload2.Gateways[0].GatewayID != "ph-xconnect-gateway-01" {
		t.Fatalf("expected bound gateway, got %#v err=%v", gwPayload2, err)
	}

	enrollReq2 := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/devices/enroll", bytes.NewReader(enrollBody1))
	enrollReq2.Header.Set("Authorization", "Bearer "+memberTokens.AccessToken)
	enrollReq2.Header.Set("Content-Type", "application/json")
	enrollResp2 := httptest.NewRecorder()
	router.ServeHTTP(enrollResp2, enrollReq2)
	if enrollResp2.Code != http.StatusOK {
		t.Fatalf("POST enroll after bind status=%d body=%s", enrollResp2.Code, enrollResp2.Body.String())
	}
	var exchange overlay.ExchangeResponse
	if err := json.Unmarshal(enrollResp2.Body.Bytes(), &exchange); err != nil || exchange.Device.ID != "one-member-1" || exchange.Device.UserID != member.ID {
		t.Fatalf("unexpected enroll response: %#v err=%v", exchange, err)
	}

	// 7. Owner unbinds member.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/overlay/v1/admin/networks/net-ph-xconnect/members/"+member.ID, nil)
	delReq.Header.Set("Authorization", "Bearer "+ownerTokens.AccessToken)
	delResp := httptest.NewRecorder()
	router.ServeHTTP(delResp, delReq)
	if delResp.Code != http.StatusOK {
		t.Fatalf("DELETE member status=%d body=%s", delResp.Code, delResp.Body.String())
	}
	var delPayload struct {
		Deleted   bool   `json:"deleted"`
		NetworkID string `json:"network_id"`
		UserID    string `json:"user_id"`
	}
	if err := json.Unmarshal(delResp.Body.Bytes(), &delPayload); err != nil || !delPayload.Deleted || delPayload.NetworkID != "net-ph-xconnect" || delPayload.UserID != member.ID {
		t.Fatalf("unexpected delete payload: %#v err=%v", delPayload, err)
	}

	// 8. Unbound member cannot enroll another device.
	oneKey2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{10}, 32))
	enrollBody2, _ := json.Marshal(overlay.UserEnrollRequest{
		NetworkID: "net-ph-xconnect", GatewayID: "ph-xconnect-gateway-01", DeviceID: "one-member-2",
		Platform: "darwin", WireGuardPublicKey: oneKey2,
	})
	enrollReq3 := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/devices/enroll", bytes.NewReader(enrollBody2))
	enrollReq3.Header.Set("Authorization", "Bearer "+memberTokens.AccessToken)
	enrollReq3.Header.Set("Content-Type", "application/json")
	enrollResp3 := httptest.NewRecorder()
	router.ServeHTTP(enrollResp3, enrollReq3)
	if enrollResp3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 after unbinding, got %d body=%s", enrollResp3.Code, enrollResp3.Body.String())
	}
}
