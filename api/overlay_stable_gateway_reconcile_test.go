package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/internal/overlay"
	"account/internal/store"
)

func TestInternalStableGatewayReconcileRequiresServiceTokenAndAuditsRepair(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-token")
	db, err := gorm.Open(sqlite.Open("file:api-stable-gateway-reconcile?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := overlay.NewService(db, overlay.Config{SigningPrivateKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	owner := &store.User{ID: "11111111-1111-4111-8111-111111111111", Name: "uat-owner", Email: "uat-owner@example.test", Active: true}
	st := store.NewMemoryStore()
	if err := st.CreateUser(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Seed(context.Background(), overlay.BootstrapConfig{
		Network: overlay.BootstrapNetwork{ID: "net_uat", DisplayName: "UAT", CIDR: "10.77.0.0/24", GatewayID: "gw-uat-tw-xconnect", GatewayWireGuardKey: "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=", GatewayWireGuardAddress: "10.77.0.1/32", GatewayEndpointHost: "tw-xconnect.svc.plus", GatewayEndpointPort: 443, TransportServerName: "tw-xconnect.svc.plus", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", TransportKind: overlay.TransportVLESSXHTTP, TransportPath: "/xconnect", TransportMode: "auto", TransportHost: "tw-xconnect.svc.plus", OwnerUserID: "old-owner"},
		Invite:  overlay.BootstrapInvite{DeviceID: "gateway-uat", Platform: "linux", Role: overlay.RoleGateway, ExpiresAt: time.Now().Add(time.Hour)},
	}, "api-stable-token"); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithOverlayService(service))
	body := []byte(`{"environment":"uat","network_id":"net_uat","gateway_id":"gw-uat-tw-xconnect","gateway_endpoint_host":"tw-xconnect.svc.plus","owner_email":"uat-owner@example.test"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/gateways/reconcile-stable-owner", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected service-token protection, got %d", unauthorized.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/internal/overlay/gateways/reconcile-stable-owner", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Service-Token", "internal-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected reconciliation success, got %d: %s", response.Code, response.Body.String())
	}
	var result overlay.StableGatewayReconcileResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OwnerReconciled || result.OwnerUserID != owner.ID {
		t.Fatalf("unexpected result: %#v", result)
	}
	entries, err := st.ListAuditLogs(context.Background(), store.AuditLogFilter{ActionPrefix: store.AuditActionOverlayOwnerReconcile})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one reconciliation audit entry, got %d", len(entries))
	}
	if entries[0].ActorUUID != "" {
		t.Fatalf("system reconciliation must retain a nullable audit actor, got %q", entries[0].ActorUUID)
	}
	if entries[0].Details["reason"] != "UAT stable Gateway ownership reconciliation" {
		t.Fatalf("unexpected audit details: %#v", entries[0].Details)
	}
}
