package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
)

func TestXWorkmateVaultLiveIntegration(t *testing.T) {
	vaultAddr := strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_ADDR"))
	vaultToken := strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_TOKEN"))
	vaultMount := strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_MOUNT"))
	if vaultAddr == "" || vaultToken == "" {
		t.Skip("live vault integration requires XWORKMATE_VAULT_ADDR and XWORKMATE_VAULT_TOKEN")
	}
	if vaultMount == "" {
		vaultMount = "kv"
	}

	vaultService, err := NewXWorkmateVaultService(XWorkmateVaultConfig{
		Address:   vaultAddr,
		Token:     vaultToken,
		Namespace: strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_NAMESPACE")),
		Mount:     vaultMount,
	})
	if err != nil {
		t.Fatalf("create vault service: %v", err)
	}

	router, _, token := newXWorkmateTestHarnessWithVault(t, nil, vaultService)

	profileBody, err := json.Marshal(map[string]any{
		"profile": map[string]any{
			"openclawUrl":    "wss://gateway.example.com",
			"openclawOrigin": "https://gateway.example.com",
			"vaultUrl":       vaultAddr,
			"vaultNamespace": strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_NAMESPACE")),
			"apisixUrl":      "https://apigw.example.com",
		},
	})
	if err != nil {
		t.Fatalf("marshal profile payload: %v", err)
	}

	putProfileReq := httptest.NewRequest(http.MethodPut, "/api/auth/xworkmate/profile", bytes.NewReader(profileBody))
	putProfileReq.Header.Set("Content-Type", "application/json")
	putProfileReq.Header.Set("Authorization", "Bearer "+token)
	putProfileReq.Header.Set("X-Forwarded-Host", "shared.test.invalid")
	putProfileRec := httptest.NewRecorder()
	router.ServeHTTP(putProfileRec, putProfileReq)
	if putProfileRec.Code != http.StatusOK {
		t.Fatalf("expected profile update success, got %d: %s", putProfileRec.Code, putProfileRec.Body.String())
	}

	targets := []string{
		store.XWorkmateSecretLocatorTargetOpenclawGatewayToken,
		store.XWorkmateSecretLocatorTargetVaultRootToken,
		store.XWorkmateSecretLocatorTargetAIGatewayAccessToken,
	}
	secretValuePrefix := "live-vault-check-" + time.Now().UTC().Format("20060102T150405.000000000")

	for _, target := range targets {
		body, err := json.Marshal(map[string]any{"value": secretValuePrefix + "-" + target})
		if err != nil {
			t.Fatalf("marshal secret payload for %s: %v", target, err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/auth/xworkmate/secrets/"+target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Host", "shared.test.invalid")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected live secret write success for %s, got %d: %s", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), secretValuePrefix) {
			t.Fatalf("expected live secret write response to hide raw value for %s", target)
		}
	}

	getSecretsReq := httptest.NewRequest(http.MethodGet, "/api/auth/xworkmate/secrets", nil)
	getSecretsReq.Header.Set("Authorization", "Bearer "+token)
	getSecretsReq.Header.Set("X-Forwarded-Host", "shared.test.invalid")
	getSecretsRec := httptest.NewRecorder()
	router.ServeHTTP(getSecretsRec, getSecretsReq)
	if getSecretsRec.Code != http.StatusOK {
		t.Fatalf("expected live secret status fetch success, got %d: %s", getSecretsRec.Code, getSecretsRec.Body.String())
	}
	if strings.Contains(getSecretsRec.Body.String(), secretValuePrefix) {
		t.Fatalf("expected live secret status response to hide raw values")
	}

	var getSecretsResp struct {
		TokenConfigured struct {
			Openclaw bool `json:"openclaw"`
			Vault    bool `json:"vault"`
			Apisix   bool `json:"apisix"`
		} `json:"tokenConfigured"`
	}
	if err := json.Unmarshal(getSecretsRec.Body.Bytes(), &getSecretsResp); err != nil {
		t.Fatalf("decode live secret status response: %v", err)
	}
	if !getSecretsResp.TokenConfigured.Openclaw || !getSecretsResp.TokenConfigured.Vault || !getSecretsResp.TokenConfigured.Apisix {
		t.Fatalf("expected all live tokenConfigured statuses true, got %#v", getSecretsResp.TokenConfigured)
	}

	for _, target := range targets {
		req := httptest.NewRequest(http.MethodDelete, "/api/auth/xworkmate/secrets/"+target, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Host", "shared.test.invalid")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected live secret delete success for %s, got %d: %s", target, rec.Code, rec.Body.String())
		}
	}

	getProfileReq := httptest.NewRequest(http.MethodGet, "/api/auth/xworkmate/profile", nil)
	getProfileReq.Header.Set("Authorization", "Bearer "+token)
	getProfileReq.Header.Set("X-Forwarded-Host", "shared.test.invalid")
	getProfileRec := httptest.NewRecorder()
	router.ServeHTTP(getProfileRec, getProfileReq)
	if getProfileRec.Code != http.StatusOK {
		t.Fatalf("expected live profile fetch success, got %d: %s", getProfileRec.Code, getProfileRec.Body.String())
	}

	var profileResp struct {
		TokenConfigured struct {
			Openclaw bool `json:"openclaw"`
			Vault    bool `json:"vault"`
			Apisix   bool `json:"apisix"`
		} `json:"tokenConfigured"`
	}
	if err := json.Unmarshal(getProfileRec.Body.Bytes(), &profileResp); err != nil {
		t.Fatalf("decode live profile response: %v", err)
	}
	if profileResp.TokenConfigured.Openclaw || profileResp.TokenConfigured.Vault || profileResp.TokenConfigured.Apisix {
		t.Fatalf("expected live profile tokenConfigured statuses to reset after cleanup, got %#v", profileResp.TokenConfigured)
	}
}

func TestXWorkmateVaultLiveIntegrationPrivateScope(t *testing.T) {
	vaultAddr := strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_ADDR"))
	vaultToken := strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_TOKEN"))
	vaultMount := strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_MOUNT"))
	if vaultAddr == "" || vaultToken == "" {
		t.Skip("live vault integration requires XWORKMATE_VAULT_ADDR and XWORKMATE_VAULT_TOKEN")
	}
	if vaultMount == "" {
		vaultMount = "kv"
	}

	vaultService, err := NewXWorkmateVaultService(XWorkmateVaultConfig{
		Address:   vaultAddr,
		Token:     vaultToken,
		Namespace: strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_NAMESPACE")),
		Mount:     vaultMount,
	})
	if err != nil {
		t.Fatalf("create vault service: %v", err)
	}

	ctx := context.Background()
	st := store.NewMemoryStore()

	tenantID := "sandbox-live-" + time.Now().UTC().Format("20060102t150405000000000")
	tenantDomain := tenantID + ".svc.plus"
	tenant := &store.Tenant{
		ID:      tenantID,
		Name:    "Sandbox Live Tenant",
		Edition: store.TenantPrivateEdition,
	}
	if err := st.EnsureTenant(ctx, tenant); err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}
	if err := st.EnsureTenantDomain(ctx, &store.TenantDomain{
		TenantID:  tenant.ID,
		Domain:    tenantDomain,
		Kind:      store.TenantDomainKindGenerated,
		IsPrimary: true,
		Status:    store.TenantDomainStatusVerified,
	}); err != nil {
		t.Fatalf("ensure tenant domain: %v", err)
	}

	user := &store.User{
		Name:          "Vault Sandbox Operator",
		Email:         "vault-sandbox-operator@example.com",
		EmailVerified: true,
		Role:          store.RoleAdmin,
		Level:         store.LevelAdmin,
		Active:        true,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.UpsertTenantMembership(ctx, &store.TenantMembership{
		TenantID: tenant.ID,
		UserID:   user.ID,
		Role:     store.TenantMembershipRoleAdmin,
	}); err != nil {
		t.Fatalf("upsert tenant membership: %v", err)
	}

	token := "sandbox-live-token"
	if err := st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	engine := newPrivateScopeLiveRouter(t, st, vaultService)

	profileBody, err := json.Marshal(map[string]any{
		"profile": map[string]any{
			"openclawUrl":    "wss://gateway.example.com",
			"openclawOrigin": "https://gateway.example.com",
			"vaultUrl":       vaultAddr,
			"vaultNamespace": strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_NAMESPACE")),
			"apisixUrl":      "https://apigw.example.com",
		},
	})
	if err != nil {
		t.Fatalf("marshal profile payload: %v", err)
	}

	putProfileReq := httptest.NewRequest(http.MethodPut, "/api/auth/xworkmate/profile", bytes.NewReader(profileBody))
	putProfileReq.Header.Set("Content-Type", "application/json")
	putProfileReq.Header.Set("Authorization", "Bearer "+token)
	putProfileReq.Header.Set("X-Forwarded-Host", tenantDomain)
	putProfileRec := httptest.NewRecorder()
	engine.ServeHTTP(putProfileRec, putProfileReq)
	if putProfileRec.Code != http.StatusOK {
		t.Fatalf("expected private-scope profile update success, got %d: %s", putProfileRec.Code, putProfileRec.Body.String())
	}

	targets := []string{
		store.XWorkmateSecretLocatorTargetOpenclawGatewayToken,
		store.XWorkmateSecretLocatorTargetVaultRootToken,
		store.XWorkmateSecretLocatorTargetAIGatewayAccessToken,
	}
	secretValuePrefix := "private-live-vault-check-" + time.Now().UTC().Format("20060102T150405.000000000")

	for _, target := range targets {
		body, err := json.Marshal(map[string]any{"value": secretValuePrefix + "-" + target})
		if err != nil {
			t.Fatalf("marshal secret payload for %s: %v", target, err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/auth/xworkmate/secrets/"+target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Host", tenantDomain)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected private-scope secret write success for %s, got %d: %s", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), secretValuePrefix) {
			t.Fatalf("expected private-scope secret write response to hide raw value for %s", target)
		}
	}

	getSecretsReq := httptest.NewRequest(http.MethodGet, "/api/auth/xworkmate/secrets", nil)
	getSecretsReq.Header.Set("Authorization", "Bearer "+token)
	getSecretsReq.Header.Set("X-Forwarded-Host", tenantDomain)
	getSecretsRec := httptest.NewRecorder()
	engine.ServeHTTP(getSecretsRec, getSecretsReq)
	if getSecretsRec.Code != http.StatusOK {
		t.Fatalf("expected private-scope secret status fetch success, got %d: %s", getSecretsRec.Code, getSecretsRec.Body.String())
	}

	var getSecretsResp struct {
		ProfileScope    string `json:"profileScope"`
		TokenConfigured struct {
			Openclaw bool `json:"openclaw"`
			Vault    bool `json:"vault"`
			Apisix   bool `json:"apisix"`
		} `json:"tokenConfigured"`
	}
	if err := json.Unmarshal(getSecretsRec.Body.Bytes(), &getSecretsResp); err != nil {
		t.Fatalf("decode private-scope secret status response: %v", err)
	}
	if getSecretsResp.ProfileScope != store.XWorkmateProfileScopeUserPrivate {
		t.Fatalf("expected private profile scope, got %q", getSecretsResp.ProfileScope)
	}
	if !getSecretsResp.TokenConfigured.Openclaw || !getSecretsResp.TokenConfigured.Vault || !getSecretsResp.TokenConfigured.Apisix {
		t.Fatalf("expected all private tokenConfigured statuses true, got %#v", getSecretsResp.TokenConfigured)
	}

	for _, target := range targets {
		req := httptest.NewRequest(http.MethodDelete, "/api/auth/xworkmate/secrets/"+target, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Host", tenantDomain)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected private-scope secret delete success for %s, got %d: %s", target, rec.Code, rec.Body.String())
		}
	}

	getProfileReq := httptest.NewRequest(http.MethodGet, "/api/auth/xworkmate/profile", nil)
	getProfileReq.Header.Set("Authorization", "Bearer "+token)
	getProfileReq.Header.Set("X-Forwarded-Host", tenantDomain)
	getProfileRec := httptest.NewRecorder()
	engine.ServeHTTP(getProfileRec, getProfileReq)
	if getProfileRec.Code != http.StatusOK {
		t.Fatalf("expected private-scope profile fetch success, got %d: %s", getProfileRec.Code, getProfileRec.Body.String())
	}

	var profileResp struct {
		ProfileScope    string `json:"profileScope"`
		TokenConfigured struct {
			Openclaw bool `json:"openclaw"`
			Vault    bool `json:"vault"`
			Apisix   bool `json:"apisix"`
		} `json:"tokenConfigured"`
	}
	if err := json.Unmarshal(getProfileRec.Body.Bytes(), &profileResp); err != nil {
		t.Fatalf("decode private-scope profile response: %v", err)
	}
	if profileResp.ProfileScope != store.XWorkmateProfileScopeUserPrivate {
		t.Fatalf("expected private profile scope after cleanup, got %q", profileResp.ProfileScope)
	}
	if profileResp.TokenConfigured.Openclaw || profileResp.TokenConfigured.Vault || profileResp.TokenConfigured.Apisix {
		t.Fatalf("expected private profile tokenConfigured statuses to reset after cleanup, got %#v", profileResp.TokenConfigured)
	}
}

func newPrivateScopeLiveRouter(t *testing.T, st store.Store, vaultService xworkmateVaultService) *gin.Engine {
	t.Helper()

	router := gin.New()
	RegisterRoutes(
		router,
		WithStore(st),
		WithEmailVerification(false),
		WithTokenService(auth.NewTokenService(auth.TokenConfig{
			PublicToken:   "public-token",
			RefreshSecret: "refresh-secret",
			AccessSecret:  "access-secret",
			AccessExpiry:  time.Hour,
			RefreshExpiry: time.Hour,
			Store:         st,
		})),
		WithXWorkmateVaultService(vaultService),
	)
	return router
}
