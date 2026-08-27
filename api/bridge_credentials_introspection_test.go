package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
)

func TestBridgeCredentialIntrospectionReturnsStableAccountID(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test-token")
	h := &handler{bridgeCredentials: map[string]memoryBridgeCredential{
		"account-1|tenant-1": {
			CredentialUUID: "credential-1", Token: "bridge-user-token",
			AccountID: "account-1", TenantID: "tenant-1",
		},
	}}
	router := gin.New()
	router.POST("/api/internal/bridge/credentials/introspect", auth.InternalAuthMiddleware(), h.introspectBridgeCredential)
	request := httptest.NewRequest(http.MethodPost, "/api/internal/bridge/credentials/introspect", bytes.NewBufferString(`{"token":"bridge-user-token"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Service-Token", "internal-test-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Active       bool   `json:"active"`
		AccountID    string `json:"accountId"`
		TenantID     string `json:"tenantId"`
		CredentialID string `json:"credentialId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.Active || payload.AccountID != "account-1" || payload.TenantID != "tenant-1" || payload.CredentialID != "credential-1" {
		t.Fatalf("unexpected introspection principal: %+v", payload)
	}
}
