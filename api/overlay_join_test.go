package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/overlay/domain"
)

type overlayJoinCreateResponse struct {
	JoinToken struct {
		ID      string `json:"id"`
		JoinURI string `json:"join_uri"`
	} `json:"join_token"`
}

type overlayJoinExchangeResponse struct {
	EnrollmentToken string `json:"enrollment_token"`
	Device          struct {
		ID        string `json:"id"`
		NetworkID string `json:"network_id"`
	} `json:"device"`
}

func createJoinInvite(t *testing.T, router http.Handler, accountToken, body string) overlayJoinCreateResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+accountToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create join invite: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("join secret response is cacheable: %#v", recorder.Header())
	}
	var response overlayJoinCreateResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func joinSecretFromURI(t *testing.T, joinURI string) string {
	t.Helper()
	parsed, err := url.Parse(joinURI)
	if err != nil || parsed.Scheme != "xconnect" || parsed.Host != "join" || parsed.Query().Get("controller") == "" {
		t.Fatalf("invalid join URI %q: %v", joinURI, err)
	}
	return strings.TrimPrefix(parsed.Path, "/")
}

func exchangeJoinInvite(t *testing.T, router http.Handler, secret, deviceID, platform string) (*httptest.ResponseRecorder, overlayJoinExchangeResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"join_token": secret, "device_id": deviceID, "platform": platform,
		"wireguard_public_key": "jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
	})
	request := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens/exchange", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var response overlayJoinExchangeResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	return recorder, response
}

func TestOverlayControllerURLFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		controller string
		allowHTTP  string
		want       string
		wantError  bool
	}{
		{name: "https", controller: "https://controller.example.test/root/", want: "https://controller.example.test/root"},
		{name: "userinfo", controller: "https://user@controller.example.test", wantError: true},
		{name: "query", controller: "https://controller.example.test?tenant=one", wantError: true},
		{name: "fragment", controller: "https://controller.example.test#join", wantError: true},
		{name: "localhost production", controller: "http://localhost:8080", wantError: true},
		{name: "localhost explicit development", controller: "http://localhost:8080/", allowHTTP: "true", want: "http://localhost:8080"},
		{name: "remote http despite flag", controller: "http://controller.example.test", allowHTTP: "true", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OVERLAY_CONTROLLER_URL", test.controller)
			t.Setenv("OVERLAY_ALLOW_INSECURE_LOCALHOST", test.allowHTTP)
			got, err := (&handler{}).overlayControllerURL()
			if test.wantError && err == nil {
				t.Fatalf("expected error, got %q", got)
			}
			if !test.wantError && (err != nil || got != test.want) {
				t.Fatalf("overlayControllerURL() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestOverlayJoinExchangeAtomicallyRegistersAndScopesEnrollment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OVERLAY_CONTROLLER_URL", "https://controller.example.test")
	t.Setenv("OVERLAY_TRANSPORT_UUID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("XWORKMATE_BRIDGE_SERVER_URL", "https://bridge-uat.onwalk.net")
	service, _, _ := newSignedConfigTestService(t)
	tokenService := auth.NewTokenService(auth.TokenConfig{AccessSecret: "join-test-access", RefreshSecret: "join-test-refresh", AccessExpiry: time.Hour, RefreshExpiry: time.Hour})
	router, _, accountToken := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service), WithTokenService(tokenService))
	invite := createJoinInvite(t, router, accountToken, `{"network_id":"xworkmate-private","device_id":"joined-device","platform":"linux"}`)
	secret := joinSecretFromURI(t, invite.JoinToken.JoinURI)
	recorder, exchange := exchangeJoinInvite(t, router, secret, "joined-device", "linux")
	if recorder.Code != http.StatusOK || exchange.EnrollmentToken == "" || exchange.Device.ID != "joined-device" {
		t.Fatalf("exchange: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("exchange leaked/cached join secret: %#v %s", recorder.Header(), recorder.Body.String())
	}

	configRequest := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/config", nil)
	configRequest.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	configRecorder := httptest.NewRecorder()
	router.ServeHTTP(configRecorder, configRequest)
	if configRecorder.Code != http.StatusOK || !strings.Contains(configRecorder.Body.String(), `"runtime":"xray-core"`) {
		t.Fatalf("enrollment config: %d %s", configRecorder.Code, configRecorder.Body.String())
	}

	signedRequest := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config", nil)
	signedRequest.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	signedRecorder := httptest.NewRecorder()
	router.ServeHTTP(signedRecorder, signedRequest)
	if signedRecorder.Code != http.StatusOK || !strings.Contains(signedRecorder.Body.String(), `"proxy_core":"xray"`) {
		t.Fatalf("enrollment signed config: %d %s", signedRecorder.Code, signedRecorder.Body.String())
	}
	signedConfig, err := domain.DecodeSignedConfig(signedRecorder.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	ackBody, _ := json.Marshal(map[string]any{"config_id": signedConfig.ConfigID, "device_id": signedConfig.DeviceID})
	ackRequest := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/enrollment/signed-config/1/ack", bytes.NewReader(ackBody))
	ackRequest.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	ackRecorder := httptest.NewRecorder()
	router.ServeHTTP(ackRecorder, ackRequest)
	if ackRecorder.Code != http.StatusOK {
		t.Fatalf("enrollment signed ACK: %d %s", ackRecorder.Code, ackRecorder.Body.String())
	}

	wrongDevice := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/config?device_id=another-device", nil)
	wrongDevice.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	wrongRecorder := httptest.NewRecorder()
	router.ServeHTTP(wrongRecorder, wrongDevice)
	if wrongRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-device scope=%d %s", wrongRecorder.Code, wrongRecorder.Body.String())
	}
	wrongNetwork := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/config?network_id=other-network", nil)
	wrongNetwork.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	wrongNetworkRecorder := httptest.NewRecorder()
	router.ServeHTTP(wrongNetworkRecorder, wrongNetwork)
	if wrongNetworkRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-network scope=%d %s", wrongNetworkRecorder.Code, wrongNetworkRecorder.Body.String())
	}

	accountRequest := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	accountRequest.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	accountRecorder := httptest.NewRecorder()
	router.ServeHTTP(accountRecorder, accountRequest)
	if accountRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("enrollment accessed account API: %d %s", accountRecorder.Code, accountRecorder.Body.String())
	}
	listRequest := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/devices", nil)
	listRequest.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("enrollment accessed device list: %d %s", listRecorder.Code, listRecorder.Body.String())
	}

	replay, _ := exchangeJoinInvite(t, router, secret, "different-device", "linux")
	if replay.Code != http.StatusUnauthorized || strings.Contains(replay.Body.String(), secret) {
		t.Fatalf("join replay=%d %s", replay.Code, replay.Body.String())
	}

	// Existing account sessions remain valid and can read the atomically
	// registered device through the legacy endpoint.
	legacy := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/config?device_id=joined-device", nil)
	legacy.Header.Set("Authorization", "Bearer "+accountToken)
	legacyRecorder := httptest.NewRecorder()
	router.ServeHTTP(legacyRecorder, legacy)
	if legacyRecorder.Code != http.StatusOK {
		t.Fatalf("legacy account session regressed: %d %s", legacyRecorder.Code, legacyRecorder.Body.String())
	}
}

func TestOverlayJoinRevocationAndConstraints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OVERLAY_CONTROLLER_URL", "https://controller.example.test")
	router, _, accountToken := newAuthenticatedSyncHarness(t)
	invite := createJoinInvite(t, router, accountToken, `{"device_id":"allowed-device","platform":"linux"}`)
	secret := joinSecretFromURI(t, invite.JoinToken.JoinURI)
	mismatch, _ := exchangeJoinInvite(t, router, secret, "other-device", "linux")
	if mismatch.Code != http.StatusForbidden {
		t.Fatalf("device constraint=%d %s", mismatch.Code, mismatch.Body.String())
	}

	revoke := httptest.NewRequest(http.MethodDelete, "/api/overlay/v1/join-tokens/"+invite.JoinToken.ID, nil)
	revoke.Header.Set("Authorization", "Bearer "+accountToken)
	revokeRecorder := httptest.NewRecorder()
	router.ServeHTTP(revokeRecorder, revoke)
	if revokeRecorder.Code != http.StatusNoContent {
		t.Fatalf("revoke=%d %s", revokeRecorder.Code, revokeRecorder.Body.String())
	}
	revoked, _ := exchangeJoinInvite(t, router, secret, "allowed-device", "linux")
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked exchange=%d %s", revoked.Code, revoked.Body.String())
	}
}

func TestOverlayJoinRejectsMultiUseCreation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OVERLAY_CONTROLLER_URL", "https://controller.example.test")
	router, _, accountToken := newAuthenticatedSyncHarness(t)
	request := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens", bytes.NewBufferString(`{"remaining_uses":2}`))
	request.Header.Set("Authorization", "Bearer "+accountToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_remaining_uses") {
		t.Fatalf("multi-use invite accepted: %d %s", recorder.Code, recorder.Body.String())
	}
}

type denyOverlayJoinLimiter struct{}

func (denyOverlayJoinLimiter) Allow(string, time.Time) bool { return false }

func TestOverlayJoinExchangeRateLimiterIntegrationPoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, _, _ := newAuthenticatedSyncHarness(t, WithOverlayJoinRateLimiter(denyOverlayJoinLimiter{}))
	recorder, _ := exchangeJoinInvite(t, router, "xjt_not-real-but-long-enough-to-reach-limiter", "device", "linux")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limiter status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
