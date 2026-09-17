package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/overlay"
	"account/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestInternalStableGatewayOwnerReconciliation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "test-internal-token")

	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:api-stable-gateway-reconcile-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	overlayService, err := overlay.NewService(db, overlay.Config{SigningPrivateKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	accounts := store.NewMemoryStore()
	if err := accounts.CreateUser(t.Context(), &store.User{
		ID: "portal-owner", Name: "Portal Owner", Email: "admin@example.com", PasswordHash: "hashed",
		EmailVerified: true, Role: store.RoleAdmin, Level: store.LevelAdmin, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := overlayService.Seed(t.Context(), overlay.BootstrapConfig{Network: overlay.BootstrapNetwork{
		ID: "net_uat", DisplayName: "UAT", CIDR: "10.95.0.0/29", GatewayID: "gw-uat-tw-xconnect",
		GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), GatewayWireGuardAddress: "10.95.0.1/32",
		GatewayEndpointHost: "tw-xconnect.svc.plus", GatewayEndpointPort: 443, TransportServerName: "tw-xconnect.svc.plus",
		TransportPort: 443, TransportAuthID: "33333333-3333-3333-3333-333333333333", OwnerUserID: "old-owner",
	}, Invite: overlay.BootstrapInvite{Platform: "linux", Role: overlay.RoleGateway, ExpiresAt: time.Now().UTC().Add(time.Hour)}}, "seed-token"); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(accounts), WithOverlayService(overlayService), WithEmailVerification(false))
	payload, err := json.Marshal(map[string]string{
		"environment": "uat", "network_id": "net_uat", "gateway_id": "gw-uat-tw-xconnect",
		"gateway_endpoint_host": "tw-xconnect.svc.plus", "owner_email": "admin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/gateways/reconcile-stable-owner", bytes.NewReader(payload))
	req.Header.Set("X-Service-Token", "test-internal-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconciliation status: %d body=%s", rec.Code, rec.Body.String())
	}
	var response overlay.StableGatewayOwnerReconciliationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OwnerReconciled || response.NetworkID != "net_uat" || response.GatewayID != "gw-uat-tw-xconnect" {
		t.Fatalf("unexpected reconciliation response: %#v", response)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/internal/overlay/gateways/reconcile-stable-owner", bytes.NewReader([]byte(`{"environment":"prod","network_id":"net_uat","gateway_id":"gw-uat-tw-xconnect","gateway_endpoint_host":"tw-xconnect.svc.plus","owner_email":"admin@example.com"}`)))
	req.Header.Set("X-Service-Token", "test-internal-token")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected prod reconciliation rejection, got %d body=%s", rec.Code, rec.Body.String())
	}
}
