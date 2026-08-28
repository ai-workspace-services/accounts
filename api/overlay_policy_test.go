package api

import (
	"account/internal/auth"
	"account/internal/overlay/acl"
	"account/internal/store"
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func policyAPIHarness(t *testing.T) (*gin.Engine, store.Store, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	user := &store.User{Name: "Policy Admin", Email: "policy@example.com", EmailVerified: true, Role: store.RoleReadOnly, Level: store.LevelUser, Permissions: []string{permissionAdminSettingsRead, permissionAdminSettingsWrite}, Active: true}
	if err := st.CreateUser(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	device := &store.OverlayDevice{ID: "dev-a", UserID: user.ID, NetworkID: "net", Name: "a", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", WireGuardAddress: "10.77.0.10/32"}
	if err := st.UpsertOverlayDevice(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokenService(auth.TokenConfig{AccessSecret: "policy-access-secret", RefreshSecret: "policy-refresh-secret", AccessExpiry: time.Hour, RefreshExpiry: time.Hour})
	pair, err := tokens.GenerateTokenPair(user.ID, user.Email, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	RegisterRoutes(r, WithStore(st), WithTokenService(tokens))
	return r, st, pair.AccessToken
}
func policySource() string {
	return `{"apiVersion":"overlay.xconnect.svc.plus/v1alpha1","kind":"NetworkPolicy","metadata":{"name":"deny-default"},"spec":{"defaultAction":"deny","tagOwners":{"tag:self":["user:policy@example.com"]},"rules":[{"id":"self","action":"accept","sources":["device:dev-a"],"destinations":["tag:self"],"protocols":["tcp"],"ports":[443]}]}}`
}
func policyRequest(t *testing.T, r http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}
func TestPolicyManagementLifecycleAndFailClosedPermission(t *testing.T) {
	r, _, token := policyAPIHarness(t)
	bodyBytes, _ := json.Marshal(map[string]any{"network_id": "net", "source": policySource(), "source_format": "json"})
	body := string(bodyBytes)
	if rec := policyRequest(t, r, "", http.MethodPost, "/api/overlay/v1/policies/validate", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth=%d", rec.Code)
	}
	rec := policyRequest(t, r, token, http.MethodPost, "/api/overlay/v1/policies/validate", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate=%d %s", rec.Code, rec.Body.String())
	}
	rec = policyRequest(t, r, token, http.MethodPost, "/api/overlay/v1/policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rec.Code, rec.Body.String())
	}
	rec = policyRequest(t, r, token, http.MethodPost, "/api/overlay/v1/policies/1/activate", `{"network_id":"net"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"generation":2`) {
		t.Fatalf("activate=%d %s", rec.Code, rec.Body.String())
	}
	rec = policyRequest(t, r, token, http.MethodPut, "/api/overlay/v1/devices/dev-a/tags", `{"network_id":"net","tags":["TAG:self"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"policy_generation":3`) || !strings.Contains(rec.Body.String(), `"tags":["tag:self"]`) {
		t.Fatalf("tag=%d %s", rec.Code, rec.Body.String())
	}
	rec = policyRequest(t, r, token, http.MethodPost, "/api/overlay/v1/policies/1/explain", `{"network_id":"net","source":"device:dev-a","destination":"device:dev-a","protocol":"tcp","port":443}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"accept"`) {
		t.Fatalf("explain=%d %s", rec.Code, rec.Body.String())
	}
}
func TestGatewayPolicyArtifactBindingAndNoPII(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test")
	r, st := gatewayAPIHarness(t)
	service, _ := acl.NewService(st)
	p, err := service.Create(t.Context(), "network-test", "11111111-1111-4111-8111-111111111111", []byte(`{"apiVersion":"overlay.xconnect.svc.plus/v1alpha1","kind":"NetworkPolicy","metadata":{"name":"deny"},"spec":{"defaultAction":"deny"}}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	p, err = service.Activate(t.Context(), "network-test", 1, "admin-b")
	if err != nil {
		t.Fatal(err)
	}
	_, bearer := createGatewayCredential(t, r)
	path := "/api/internal/overlay/v1/nodes/gw_test_01/policy-artifacts/" + strconv.FormatUint(p.Generation, 10) + "/" + p.ArtifactSHA256
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != gatewayPolicyMediaType || !bytes.Equal(rec.Body.Bytes(), p.Artifact) {
		t.Fatalf("artifact=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	bad := httptest.NewRequest(http.MethodGet, "/api/internal/overlay/v1/nodes/other/policy-artifacts/"+strconv.FormatUint(p.Generation, 10)+"/"+p.ArtifactSHA256, nil)
	bad.Header.Set("Authorization", "Bearer "+bearer)
	badRec := httptest.NewRecorder()
	r.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("cross node=%d", badRec.Code)
	}
}
