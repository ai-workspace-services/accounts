package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/overlay/domain"
	"account/internal/overlay/gatewayprojection"
	"account/internal/overlay/projection"
	"account/internal/store"
)

func gatewayAPIHarness(t *testing.T) (http.Handler, store.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	user := &store.User{ID: "11111111-1111-4111-8111-111111111111", Name: "Gateway Device Owner", Email: "gateway-owner@example.com", Active: true}
	if err := st.CreateUser(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	node := &store.OverlayNode{ID: "gw_test_01", NetworkID: "network-test", Name: "gateway", Role: "gateway", WireGuardPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), WireGuardAddress: "10.77.0.1/32", EndpointHost: "gateway.example", EndpointPort: 443, TransportType: "vless-tls", TransportSecurity: "tls", TransportUUID: "11111111-1111-4111-8111-111111111111", Healthy: true}
	if err := st.UpsertOverlayNode(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	device := &store.OverlayDevice{ID: "dev_test_01", UserID: "11111111-1111-4111-8111-111111111111", NetworkID: "network-test", Name: "device", Platform: "linux", WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), WireGuardAddress: "10.77.0.10/32"}
	if err := st.UpsertOverlayDevice(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	signer, err := projection.NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "key_test_01")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	signed, err := projection.NewService(projection.NewMemoryRepository(clock), signer, clock, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := gatewayprojection.NewService(st, signed, clock, gatewayprojection.Config{Lifetime: time.Hour, RenewalInterval: 30 * time.Minute, MaxPeerRemovalPercent: 100})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithOverlayProjectionService(signed), WithOverlayGatewayProjectionService(gateway))
	return router, st
}

func createGatewayCredential(t *testing.T, router http.Handler) (string, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/gw_test_01/credentials", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Service-Token", "internal-test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create credential=%d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential response cacheable")
	}
	var body struct {
		Credential struct {
			ID     string `json:"id"`
			Bearer string `json:"bearer_token"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Credential.ID, body.Credential.Bearer
}

func TestGatewayAgentControlPlaneAndNodeCredentialBoundary(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test")
	router, _ := gatewayAPIHarness(t)
	credentialID, bearer := createGatewayCredential(t, router)
	if !gatewayNodeSecretPattern.MatchString(bearer) {
		t.Fatalf("invalid bearer shape %q", bearer)
	}
	heartbeat := `{"node_id":"gw_test_01","agent_version":"0.1.0","mode":"shadow","proxy_core":"xray","observed_generation":0,"applied_generation":0}`
	request := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/heartbeat", strings.NewReader(heartbeat))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("heartbeat=%d %s", response.Code, response.Body.String())
	}

	serviceOnly := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/heartbeat", strings.NewReader(heartbeat))
	serviceOnly.Header.Set("Content-Type", "application/json")
	serviceOnly.Header.Set("X-Service-Token", "internal-test")
	serviceResponse := httptest.NewRecorder()
	router.ServeHTTP(serviceResponse, serviceOnly)
	if serviceResponse.Code != http.StatusUnauthorized {
		t.Fatalf("shared token reached Agent v1: %d", serviceResponse.Code)
	}
	malformed := httptest.NewRequest(http.MethodGet, "/api/internal/overlay/v1/nodes/gw_test_01/snapshot", nil)
	malformed.Header.Set("Authorization", "Bearer xgn_not-canonical")
	malformedResponse := httptest.NewRecorder()
	router.ServeHTTP(malformedResponse, malformed)
	if malformedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("malformed node secret=%d", malformedResponse.Code)
	}

	snapshotRequest := httptest.NewRequest(http.MethodGet, "/api/internal/overlay/v1/nodes/gw_test_01/snapshot", nil)
	snapshotRequest.Header.Set("Authorization", "Bearer "+bearer)
	snapshotRequest.Header.Set("Accept", gatewayMediaType)
	snapshotResponse := httptest.NewRecorder()
	router.ServeHTTP(snapshotResponse, snapshotRequest)
	if snapshotResponse.Code != http.StatusOK || !strings.HasPrefix(snapshotResponse.Header().Get("Content-Type"), gatewayMediaType) {
		t.Fatalf("snapshot=%d %#v %s", snapshotResponse.Code, snapshotResponse.Header(), snapshotResponse.Body.String())
	}
	snapshot, err := domain.DecodeGatewaySnapshot(snapshotResponse.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ProxyCore != "xray" || snapshot.Relay.Transport != "vless-tls-xudp" {
		t.Fatalf("snapshot runtime drift: %+v", snapshot)
	}

	apply := map[string]any{"node_id": "gw_test_01", "snapshot_id": snapshot.SnapshotID, "observed_generation": snapshot.Generation, "applied_generation": 0, "runtime_applied": false, "result": "shadow_validated", "diff": map[string]any{"status": "available", "equal": true, "projected_peers": 1, "current_peers": 1, "missing_peers": 0, "unexpected_peers": 0, "route_mismatches": 0}}
	raw, _ := json.Marshal(apply)
	post := func(body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/gw_test_01/apply-result", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	first := post(raw)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"duplicate":false`) {
		t.Fatalf("apply=%d %s", first.Code, first.Body.String())
	}
	retry := post(raw)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"duplicate":true`) {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	apply["result"] = "shadow_rejected"
	apply["diff"] = map[string]any{"status": "unavailable", "equal": false, "projected_peers": 1, "current_peers": 0, "missing_peers": 0, "unexpected_peers": 0, "route_mismatches": 0}
	conflictRaw, _ := json.Marshal(apply)
	conflict := post(conflictRaw)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflicting result=%d %s", conflict.Code, conflict.Body.String())
	}
	apply["snapshot_id"] = "snap_unknown_01"
	unknownRaw, _ := json.Marshal(apply)
	unknown := post(unknownRaw)
	if unknown.Code != http.StatusConflict {
		t.Fatalf("unknown snapshot=%d %s", unknown.Code, unknown.Body.String())
	}

	revoke := httptest.NewRequest(http.MethodDelete, "/api/internal/overlay/v1/nodes/gw_test_01/credentials/"+credentialID, nil)
	revoke.Header.Set("X-Service-Token", "internal-test")
	revokeResponse := httptest.NewRecorder()
	router.ServeHTTP(revokeResponse, revoke)
	if revokeResponse.Code != http.StatusNoContent {
		t.Fatalf("revoke=%d %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	snapshotResponse = httptest.NewRecorder()
	router.ServeHTTP(snapshotResponse, snapshotRequest)
	if snapshotResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer accepted=%d", snapshotResponse.Code)
	}
}

func TestGatewayDiffSummaryRejectsInconsistentEvidence(t *testing.T) {
	for _, diff := range []gatewayDiffSummary{{Status: "available", Equal: true, ProjectedPeers: 1, CurrentPeers: 1, MissingPeers: 1}, {Status: "available", Equal: false, ProjectedPeers: 1, CurrentPeers: 1}, {Status: "unavailable", Equal: false, ProjectedPeers: 1, CurrentPeers: 1}, {Status: "available", ProjectedPeers: 1, CurrentPeers: 0, MissingPeers: 0}} {
		if validGatewayDiff(diff) {
			t.Fatalf("inconsistent diff accepted: %+v", diff)
		}
	}
	if !validGatewayDiff(gatewayDiffSummary{Status: "unavailable", ProjectedPeers: 1}) {
		t.Fatal("Agent unavailable diff rejected")
	}
}

func TestGatewayApplyModeRequiresTrustedNodeAuthorizationAndKnownSuccess(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test")
	router, st := gatewayAPIHarness(t)
	_, shadowBearer := createGatewayCredential(t, router)
	shadowApply := `{"node_id":"gw_test_01","snapshot_id":"unknown","observed_generation":1,"applied_generation":1,"runtime_applied":true,"result":"applied","diff":{"status":"available","equal":true,"projected_peers":1,"current_peers":1,"missing_peers":0,"unexpected_peers":0,"route_mismatches":0}}`
	shadowRequest := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/gw_test_01/apply-result", strings.NewReader(shadowApply))
	shadowRequest.Header.Set("Authorization", "Bearer "+shadowBearer)
	shadowRequest.Header.Set("Content-Type", "application/json")
	shadowResponse := httptest.NewRecorder()
	router.ServeHTTP(shadowResponse, shadowRequest)
	if shadowResponse.Code != http.StatusBadRequest {
		t.Fatalf("shadow node forged apply: %d %s", shadowResponse.Code, shadowResponse.Body.String())
	}
	node, err := st.GetOverlayNode(t.Context(), "gw_test_01")
	if err != nil {
		t.Fatal(err)
	}
	node.GatewayMode = "apply"
	if err = st.UpsertOverlayNode(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	_, bearer := createGatewayCredential(t, router)
	snapshotRequest := httptest.NewRequest(http.MethodGet, "/api/internal/overlay/v1/nodes/gw_test_01/snapshot", nil)
	snapshotRequest.Header.Set("Authorization", "Bearer "+bearer)
	snapshotResponse := httptest.NewRecorder()
	router.ServeHTTP(snapshotResponse, snapshotRequest)
	if snapshotResponse.Code != http.StatusOK {
		t.Fatalf("snapshot=%d %s", snapshotResponse.Code, snapshotResponse.Body.String())
	}
	snapshot, err := domain.DecodeGatewaySnapshot(snapshotResponse.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"node_id": node.ID, "snapshot_id": snapshot.SnapshotID, "observed_generation": snapshot.Generation, "applied_generation": snapshot.Generation, "runtime_applied": true, "result": "applied", "diff": map[string]any{"status": "available", "equal": true, "projected_peers": 1, "current_peers": 1, "missing_peers": 0, "unexpected_peers": 0, "route_mismatches": 0}})
	resultRequest := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/gw_test_01/apply-result", bytes.NewReader(body))
	resultRequest.Header.Set("Authorization", "Bearer "+bearer)
	resultRequest.Header.Set("Content-Type", "application/json")
	resultResponse := httptest.NewRecorder()
	router.ServeHTTP(resultResponse, resultRequest)
	if resultResponse.Code != http.StatusOK {
		t.Fatalf("apply result=%d %s", resultResponse.Code, resultResponse.Body.String())
	}
	heartbeat := `{"node_id":"gw_test_01","agent_version":"0.2.0","mode":"apply","proxy_core":"xray","observed_generation":1,"applied_generation":1}`
	heartbeatRequest := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/heartbeat", strings.NewReader(heartbeat))
	heartbeatRequest.Header.Set("Authorization", "Bearer "+bearer)
	heartbeatRequest.Header.Set("Content-Type", "application/json")
	heartbeatResponse := httptest.NewRecorder()
	router.ServeHTTP(heartbeatResponse, heartbeatRequest)
	if heartbeatResponse.Code != http.StatusNoContent {
		t.Fatalf("apply heartbeat=%d %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}
	owner := "11111111-1111-4111-8111-111111111111"
	if _, _, err = st.RotateOverlayDeviceKey(t.Context(), owner, "network-test", "dev_test_01", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)), 1, &store.AuditLog{Action: store.AuditActionOverlayDeviceKeyRotate}); err != nil {
		t.Fatal(err)
	}
	snapshotResponse = httptest.NewRecorder()
	router.ServeHTTP(snapshotResponse, snapshotRequest)
	snapshot2, err := domain.DecodeGatewaySnapshot(snapshotResponse.Body.Bytes())
	if err != nil || snapshot2.Generation != 2 {
		t.Fatalf("second snapshot=%+v err=%v", snapshot2, err)
	}
	postResult := func(value map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(value)
		req := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/nodes/gw_test_01/apply-result", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	failure := map[string]any{"node_id": node.ID, "snapshot_id": snapshot2.SnapshotID, "observed_generation": 2, "applied_generation": 1, "runtime_applied": false, "result": "apply_rejected", "diff": map[string]any{"status": "unavailable", "equal": false, "projected_peers": 1, "current_peers": 0, "missing_peers": 0, "unexpected_peers": 0, "route_mismatches": 0}}
	if rec := postResult(failure); rec.Code != http.StatusOK {
		t.Fatalf("failure evidence=%d %s", rec.Code, rec.Body.String())
	}
	success := map[string]any{"node_id": node.ID, "snapshot_id": snapshot2.SnapshotID, "observed_generation": 2, "applied_generation": 2, "runtime_applied": true, "result": "applied", "diff": map[string]any{"status": "available", "equal": true, "projected_peers": 1, "current_peers": 1, "missing_peers": 0, "unexpected_peers": 0, "route_mismatches": 0}}
	if rec := postResult(success); rec.Code != http.StatusOK {
		t.Fatalf("failure-to-success transition=%d %s", rec.Code, rec.Body.String())
	}
	if rec := postResult(failure); rec.Code != http.StatusConflict {
		t.Fatalf("successful result regressed=%d %s", rec.Code, rec.Body.String())
	}
}
