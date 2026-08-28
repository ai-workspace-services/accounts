package api

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestDeviceCredentialGoldenWireVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "tests", "fixtures", "overlay", "device-credential-wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Credential string `json:"rotation_example_credential"`
		Verifier   string `json:"rotation_example_sha256"`
	}
	if err = json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(vector.Credential))
	if got := hex.EncodeToString(digest[:]); got != vector.Verifier {
		t.Fatalf("credential verifier=%s want=%s", got, vector.Verifier)
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("Authorization", "dEvIcE "+vector.Credential)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request
	id, verifier, ok := deviceAuthorization(context)
	if !ok || id != "xdcid_fedcba9876543210fedcba9876543210" || hex.EncodeToString(verifier) != vector.Verifier {
		t.Fatalf("golden authorization rejected/misbound: id=%q verifier=%x ok=%v", id, verifier, ok)
	}
}

func secureDeviceRequest(t *testing.T, router http.Handler, method, path, authorization, body string, idempotent bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	if idempotent {
		digest := sha256.Sum256([]byte(body))
		request.Header.Set("Idempotency-Key", "sha256-"+hex.EncodeToString(digest[:]))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func joinedDeviceCredential(t *testing.T) (http.Handler, overlayJoinExchangeResponse) {
	t.Helper()
	t.Setenv("OVERLAY_CONTROLLER_URL", "https://controller.example.test")
	t.Setenv("OVERLAY_TRANSPORT_UUID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("XWORKMATE_BRIDGE_SERVER_URL", "https://bridge-uat.onwalk.net")
	service, _, _ := newSignedConfigTestService(t)
	router, _, accountToken := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service))
	invite := createJoinInvite(t, router, accountToken, `{"network_id":"xworkmate-private","device_id":"durable-device","platform":"linux"}`)
	recorder, exchange := exchangeJoinInvite(t, router, joinSecretFromURI(t, invite.JoinToken.JoinURI), "durable-device", "linux")
	if recorder.Code != http.StatusOK {
		t.Fatalf("join exchange: %d %s", recorder.Code, recorder.Body.String())
	}
	return router, exchange
}

func TestDeviceCredentialSessionRotationAndTerminalRevoke(t *testing.T) {
	router, exchange := joinedDeviceCredential(t)
	raw := exchange.DeviceCredential.Credential
	nonce := "11111111-1111-4111-8111-111111111111"
	session := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", "device "+raw, `{"client_nonce":"`+nonce+`"}`, false)
	if session.Code != http.StatusOK || session.Header().Get("Cache-Control") != "no-store" || session.Header().Get("Vary") != "Authorization" {
		t.Fatalf("mint session: %d %#v %s", session.Code, session.Header(), session.Body.String())
	}
	var minted struct {
		ClientNonce     string                                                 `json:"client_nonce"`
		EnrollmentToken string                                                 `json:"enrollment_token"`
		DeviceID        string                                                 `json:"device_id"`
		NetworkID       string                                                 `json:"network_id"`
		Scope           []string                                               `json:"scope"`
		IssuedAt        time.Time                                              `json:"issued_at"`
		ExpiresAt       time.Time                                              `json:"expires_at"`
		SigningKeys     []struct{ KeyID, Algorithm, PublicKey, Status string } `json:"signing_keys"`
	}
	if err := json.Unmarshal(session.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.ClientNonce != nonce || minted.DeviceID != "durable-device" || minted.NetworkID != "xworkmate-private" || len(minted.SigningKeys) == 0 || minted.SigningKeys[0].Algorithm != "Ed25519" || minted.ExpiresAt.Sub(minted.IssuedAt) > 15*time.Minute {
		t.Fatalf("invalid minted device session: %#v", minted)
	}
	if strings.Join(minted.Scope, ",") != "overlay:config:read,overlay:config:ack" {
		t.Fatalf("session scope widened: %#v", minted.Scope)
	}
	management := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/devices", nil)
	management.Header.Set("Authorization", "Device "+raw)
	managementRecorder := httptest.NewRecorder()
	router.ServeHTTP(managementRecorder, management)
	if managementRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("device credential accessed management API: %d %s", managementRecorder.Code, managementRecorder.Body.String())
	}
	signed := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config", nil)
	signed.Header.Set("Authorization", "Bearer "+minted.EnrollmentToken)
	signedRecorder := httptest.NewRecorder()
	router.ServeHTTP(signedRecorder, signed)
	if signedRecorder.Code != http.StatusOK {
		t.Fatalf("minted session cannot read signed config: %d %s; mint=%s", signedRecorder.Code, signedRecorder.Body.String(), session.Body.String())
	}
	forbiddenLeave := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/enrollment/device/revoke", bytes.NewBufferString(`{}`))
	forbiddenLeave.Header.Set("Authorization", "Bearer "+minted.EnrollmentToken)
	forbiddenLeave.Header.Set("Content-Type", "application/json")
	forbiddenRecorder := httptest.NewRecorder()
	router.ServeHTTP(forbiddenRecorder, forbiddenLeave)
	if forbiddenRecorder.Code != http.StatusForbidden {
		t.Fatalf("short session gained revoke scope: %d %s", forbiddenRecorder.Code, forbiddenRecorder.Body.String())
	}

	newRaw := "xdc_fedcba9876543210fedcba9876543210.BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ"
	newHash := sha256.Sum256([]byte(newRaw))
	if got := hex.EncodeToString(newHash[:]); got != "00bdb1b7a7203fa88c9bd01bc87ef416cafd04d3379934ad535dda1252f0ea80" {
		t.Fatalf("IAC device credential golden verifier drifted: %s", got)
	}
	rotateBody := `{"new_credential_id":"xdcid_fedcba9876543210fedcba9876543210","new_credential_sha256":"` + hex.EncodeToString(newHash[:]) + `"}`
	rotate := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/credential/rotate", "Device "+raw, rotateBody, true)
	if rotate.Code != http.StatusOK || strings.Contains(rotate.Body.String(), newRaw) || strings.Contains(rotate.Body.String(), hex.EncodeToString(newHash[:])) {
		t.Fatalf("rotate: %d %s", rotate.Code, rotate.Body.String())
	}
	oldProbe := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", "Device "+raw, `{"client_nonce":"22222222-2222-4222-8222-222222222222"}`, false)
	if oldProbe.Code != http.StatusUnauthorized {
		t.Fatalf("replaced credential reused: %d %s", oldProbe.Code, oldProbe.Body.String())
	}
	newProbe := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", "Device "+newRaw, `{"client_nonce":"33333333-3333-4333-8333-333333333333"}`, false)
	if newProbe.Code != http.StatusOK {
		t.Fatalf("successor cannot prove lost-response rotation: %d %s; rotate=%s", newProbe.Code, newProbe.Body.String(), rotate.Body.String())
	}

	revokeBody := `{"client_nonce":"44444444-4444-4444-8444-444444444444"}`
	revoke := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/revoke", "Device "+newRaw, revokeBody, true)
	if revoke.Code != http.StatusAccepted || revoke.Header().Get("Cache-Control") != "no-store" || !strings.Contains(revoke.Body.String(), `"revoked":true`) || !strings.Contains(revoke.Body.String(), `"policy_reconcile_pending":true`) {
		t.Fatalf("revoke: %d %#v %s", revoke.Code, revoke.Header(), revoke.Body.String())
	}
	replay := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/revoke", "DEVICE "+newRaw, revokeBody, true)
	if replay.Code != http.StatusAccepted || !strings.Contains(replay.Body.String(), `"duplicate":true`) {
		t.Fatalf("terminal replay: %d %s", replay.Code, replay.Body.String())
	}
	afterRevoke := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", "Device "+newRaw, `{"client_nonce":"55555555-5555-4555-8555-555555555555"}`, false)
	if afterRevoke.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential minted session: %d %s", afterRevoke.Code, afterRevoke.Body.String())
	}
}

func TestDeviceCredentialWireAndTransportFailClosed(t *testing.T) {
	router, exchange := joinedDeviceCredential(t)
	raw := exchange.DeviceCredential.Credential
	body := `{"client_nonce":"11111111-1111-4111-8111-111111111111"}`

	for _, authorization := range []string{"Bearer " + raw, "Basic " + raw, "Device  " + raw, "Device " + raw + " ", "Device xdc_0123456789abcdef0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="} {
		recorder := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", authorization, body, false)
		if recorder.Code != http.StatusUnauthorized || strings.Contains(recorder.Body.String(), raw) {
			t.Fatalf("authorization %q was not redacted/rejected: %d %s", authorization, recorder.Code, recorder.Body.String())
		}
	}
	insecure := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/device/session", strings.NewReader(body))
	insecure.Header.Set("Authorization", "Device "+raw)
	insecure.Header.Set("Content-Type", "application/json")
	insecureRecorder := httptest.NewRecorder()
	router.ServeHTTP(insecureRecorder, insecure)
	if insecureRecorder.Code != http.StatusBadRequest || !strings.Contains(insecureRecorder.Body.String(), "https_required") {
		t.Fatalf("HTTP accepted: %d %s", insecureRecorder.Code, insecureRecorder.Body.String())
	}
	wrongContent := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/device/session", strings.NewReader(body))
	wrongContent.TLS = &tls.ConnectionState{}
	wrongContent.Header.Set("Authorization", "Device "+raw)
	wrongContent.Header.Set("Content-Type", "application/json; charset=utf-8")
	wrongRecorder := httptest.NewRecorder()
	router.ServeHTTP(wrongRecorder, wrongContent)
	if wrongRecorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-exact content type accepted: %d %s", wrongRecorder.Code, wrongRecorder.Body.String())
	}
	unknown := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", "Device "+raw, `{"client_nonce":"11111111-1111-4111-8111-111111111111","private_key":"secret"}`, false)
	if unknown.Code != http.StatusBadRequest || strings.Contains(unknown.Body.String(), "secret") {
		t.Fatalf("unknown secret field accepted/leaked: %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestDeviceSessionRequiresSigningTrustRootBeforeIssuance(t *testing.T) {
	t.Setenv("OVERLAY_CONTROLLER_URL", "https://controller.example.test")
	router, _, accountToken := newAuthenticatedSyncHarness(t)
	invite := createJoinInvite(t, router, accountToken, `{"device_id":"no-ring-device","platform":"linux"}`)
	recorder, exchange := exchangeJoinInvite(t, router, joinSecretFromURI(t, invite.JoinToken.JoinURI), "no-ring-device", "linux")
	if recorder.Code != http.StatusOK {
		t.Fatalf("join exchange: %d %s", recorder.Code, recorder.Body.String())
	}
	mint := secureDeviceRequest(t, router, http.MethodPost, "/api/overlay/v1/device/session", "Device "+exchange.DeviceCredential.Credential, `{"client_nonce":"11111111-1111-4111-8111-111111111111"}`, false)
	if mint.Code != http.StatusServiceUnavailable || !strings.Contains(mint.Body.String(), "signing_keys_unavailable") {
		t.Fatalf("session minted without trust root: %d %s", mint.Code, mint.Body.String())
	}
}
