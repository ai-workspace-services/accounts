package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/overlay/domain"
	"account/internal/overlay/projection"
	"account/internal/store"
)

func newSignedConfigTestService(t *testing.T) (*projection.Service, *projection.Ed25519Signer, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	signer, err := projection.NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "signing_key_01")
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}
	clock := func() time.Time { return now }
	service, err := projection.NewService(projection.NewMemoryRepository(clock), signer, clock, time.Hour)
	if err != nil {
		t.Fatalf("create projection service: %v", err)
	}
	return service, signer, now
}

func TestOverlayProjectionEnvironmentFailsClosedWithoutDurableRepository(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	t.Setenv("OVERLAY_SIGNING_PRIVATE_KEY", base64.StdEncoding.EncodeToString(seed))
	t.Setenv("OVERLAY_SIGNING_KEY_ID", "signing_key_01")
	t.Setenv("OVERLAY_PROJECTION_ALLOW_MEMORY", "")
	service, err := newOverlayProjectionServiceFromEnvironment()
	if err == nil || service != nil || !strings.Contains(err.Error(), "durable overlay projection repository") {
		t.Fatalf("production wiring did not fail closed: service=%v err=%v", service, err)
	}
	t.Setenv("OVERLAY_PROJECTION_ALLOW_MEMORY", "true")
	service, err = newOverlayProjectionServiceFromEnvironment()
	if err != nil || service == nil {
		t.Fatalf("explicit development memory mode failed: service=%v err=%v", service, err)
	}
}

func TestOverlayProjectionEnvironmentRejectsShortPreviousVerificationWindow(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	previous := ed25519.NewKeyFromSeed(append([]byte(nil), seed...)).Public().(ed25519.PublicKey)
	now := time.Now().UTC()
	t.Setenv("OVERLAY_SIGNING_CURRENT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(seed))
	t.Setenv("OVERLAY_SIGNING_CURRENT_KEY_ID", "key-current")
	t.Setenv("OVERLAY_PROJECTION_ALLOW_MEMORY", "true")
	t.Setenv("OVERLAY_SIGNED_CONFIG_TTL", "24h")
	t.Setenv("OVERLAY_SIGNING_PREVIOUS_KEYS_JSON", fmt.Sprintf(`[{"key_id":"key-old","public_key":%q,"not_before":%q,"not_after":%q}]`,
		base64.StdEncoding.EncodeToString(previous), now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)))
	service, err := newOverlayProjectionServiceFromEnvironment()
	if err == nil || service != nil || !strings.Contains(err.Error(), "required SignedConfig verification window") {
		t.Fatalf("short previous window accepted: service=%v err=%v", service, err)
	}
}

func TestOverlayProjectionEnvironmentCoversPersistedPreviousConfigExpiry(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	previous := ed25519.NewKeyFromSeed(append([]byte(nil), seed...)).Public().(ed25519.PublicKey)
	now := time.Now().UTC().Truncate(time.Second)
	st := store.NewMemoryStore()
	if err := st.SaveOverlaySignedConfig(t.Context(), &store.OverlaySignedConfigRecord{
		UserID: "user-1", DeviceID: "device-1", ConfigID: "config-1", NetworkID: "network-1",
		Generation: 1, SourceRevision: "source-1", SigningKeyID: "key-old", SignedPayload: []byte(`{}`),
		IssuedAt: now, ExpiresAt: now.Add(48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERLAY_SIGNING_CURRENT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(seed))
	t.Setenv("OVERLAY_SIGNING_CURRENT_KEY_ID", "key-current")
	t.Setenv("OVERLAY_PROJECTION_ALLOW_MEMORY", "true")
	t.Setenv("OVERLAY_SIGNED_CONFIG_TTL", "24h")
	t.Setenv("OVERLAY_SIGNING_PREVIOUS_KEYS_JSON", fmt.Sprintf(`[{"key_id":"key-old","public_key":%q,"not_before":%q,"not_after":%q}]`,
		base64.StdEncoding.EncodeToString(previous), now.Add(-time.Hour).Format(time.RFC3339), now.Add(30*time.Hour).Format(time.RFC3339)))
	service, err := newOverlayProjectionServiceFromEnvironment(st)
	if err == nil || service != nil || !strings.Contains(err.Error(), "required SignedConfig verification window") {
		t.Fatalf("persisted previous expiry was ignored: service=%v err=%v", service, err)
	}
}

func registerSignedConfigTestDevice(t *testing.T, router http.Handler, token string) {
	t.Helper()
	body := bytes.NewBufferString(`{
		"device_id":"signed-config-device",
		"name":"Signed Config Device",
		"wireguard_public_key":"jfHsw1HIqRQzGvfsRfdkS7BLThDbBvWMsAlJRp1kdkw=",
		"wireguard_address":"172.29.10.123/32"
	}`)
	request := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/devices/register", body)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("register device: %d %s", recorder.Code, recorder.Body.String())
	}
}

func getSignedConfig(t *testing.T, router http.Handler, token string) domain.SignedConfig {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/signed-config?device_id=signed-config-device", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get signed config: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	config, err := domain.DecodeSignedConfig(recorder.Body.Bytes())
	if err != nil {
		t.Fatalf("decode signed config: %v", err)
	}
	return config
}

func TestOverlaySignedConfigProjectionAndLegacyCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OVERLAY_TRANSPORT_UUID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("XWORKMATE_BRIDGE_SERVER_URL", "https://bridge-uat.onwalk.net")
	service, signer, now := newSignedConfigTestService(t)
	router, _, token := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service))
	registerSignedConfigTestDevice(t, router, token)

	config := getSignedConfig(t, router, token)
	if config.ProxyCore != domain.ProxyCoreXray || config.Generation != 1 {
		t.Fatalf("unexpected canonical config: %#v", config)
	}
	if err := projection.VerifySignedConfig(config, signer, now); err != nil {
		t.Fatalf("verify signed config: %v", err)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	for _, forbidden := range []string{"private_key", "refresh_token", "vault_token"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("signed config leaked %s", forbidden)
		}
	}

	legacyRequest := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/config?device_id=signed-config-device", nil)
	legacyRequest.Header.Set("Authorization", "Bearer "+token)
	legacyRecorder := httptest.NewRecorder()
	router.ServeHTTP(legacyRecorder, legacyRequest)
	if legacyRecorder.Code != http.StatusOK || !strings.Contains(legacyRecorder.Body.String(), `"revision"`) {
		t.Fatalf("legacy config regressed: %d %s", legacyRecorder.Code, legacyRecorder.Body.String())
	}
}

func TestOverlaySignedConfigRequiresAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _, _ := newSignedConfigTestService(t)
	router, _, _ := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service))
	request := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/signed-config?device_id=signed-config-device", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestOverlaySigningKeyDiscoveryPublishesOnlyVerificationMaterial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	currentSeed := make([]byte, ed25519.SeedSize)
	currentSeed[0] = 7
	previousSeed := make([]byte, ed25519.SeedSize)
	previousSeed[0] = 8
	currentPrivate := ed25519.NewKeyFromSeed(currentSeed)
	previousPrivate := ed25519.NewKeyFromSeed(previousSeed)
	current, err := projection.NewEd25519Signer(currentPrivate, "key-current")
	if err != nil {
		t.Fatal(err)
	}
	windowEnd := now.Add(24 * time.Hour)
	ring, err := projection.NewEd25519KeyRingWithCurrentSigner(current, now.Add(-time.Hour), nil, []projection.KeyRingEntry{{
		KeyID: "key-previous", PublicKey: previousPrivate.Public().(ed25519.PublicKey), Status: "previous",
		NotBefore: now.Add(-24 * time.Hour), NotAfter: &windowEnd,
	}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	service, err := projection.NewService(projection.NewMemoryRepository(func() time.Time { return now }), ring, func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router, _, token := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service))
	request := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/signing-keys", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("signing keys status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, max-age=300" || recorder.Header().Get("Vary") != "Authorization" || recorder.Header().Get("ETag") == "" {
		t.Fatalf("unsafe signing-key cache headers: %#v", recorder.Header())
	}
	body := recorder.Body.String()
	for _, required := range []string{`"key_id":"key-current"`, `"key_id":"key-previous"`, `"status":"current"`, `"status":"previous"`} {
		if !strings.Contains(body, required) {
			t.Errorf("response missing %s: %s", required, body)
		}
	}
	for _, forbidden := range []string{"private", base64.StdEncoding.EncodeToString(currentSeed), base64.StdEncoding.EncodeToString(currentPrivate)} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("signing discovery leaked private material %q", forbidden)
		}
	}
	conditional := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/signing-keys", nil)
	conditional.Header.Set("Authorization", "Bearer "+token)
	conditional.Header.Set("If-None-Match", recorder.Header().Get("ETag"))
	conditionalRecorder := httptest.NewRecorder()
	router.ServeHTTP(conditionalRecorder, conditional)
	if conditionalRecorder.Code != http.StatusNotModified || conditionalRecorder.Body.Len() != 0 {
		t.Fatalf("conditional signing-key response=%d %q", conditionalRecorder.Code, conditionalRecorder.Body.String())
	}
}

func TestOverlaySignedConfigEndpointFailsClosedWithoutRepository(t *testing.T) {
	gin.SetMode(gin.TestMode)
	seed := make([]byte, ed25519.SeedSize)
	t.Setenv("OVERLAY_SIGNING_PRIVATE_KEY", base64.StdEncoding.EncodeToString(seed))
	t.Setenv("OVERLAY_SIGNING_KEY_ID", "signing_key_01")
	t.Setenv("OVERLAY_PROJECTION_ALLOW_MEMORY", "")
	router, _, token := newAuthenticatedSyncHarness(t)
	request := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/signed-config?device_id=signed-config-device", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "overlay_projection_unavailable") {
		t.Fatalf("fail-closed status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestOverlaySignedConfigAckIsIdempotentAndRejectsStaleGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OVERLAY_TRANSPORT_UUID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("XWORKMATE_BRIDGE_SERVER_URL", "https://bridge-uat.onwalk.net")
	service, _, _ := newSignedConfigTestService(t)
	router, _, token := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service))
	registerSignedConfigTestDevice(t, router, token)
	first := getSignedConfig(t, router, token)

	ack := func(body string, generation uint64) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/signed-config/"+strconv.FormatUint(generation, 10)+"/ack", bytes.NewBufferString(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}
	body := `{"config_id":"` + first.ConfigID + `","device_id":"` + first.DeviceID + `","applied_at":"2026-08-27T12:05:00Z"}`
	firstAck := ack(body, first.Generation)
	if firstAck.Code != http.StatusOK || !strings.Contains(firstAck.Body.String(), `"duplicate":false`) {
		t.Fatalf("first ACK: %d %s", firstAck.Code, firstAck.Body.String())
	}
	duplicate := ack(body, first.Generation)
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate ACK: %d %s", duplicate.Code, duplicate.Body.String())
	}

	t.Setenv("OVERLAY_WIREGUARD_MTU", "1281")
	second := getSignedConfig(t, router, token)
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation = %d, want %d", second.Generation, first.Generation+1)
	}
	stale := ack(body, first.Generation)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "stale_generation") {
		t.Fatalf("stale ACK: %d %s", stale.Code, stale.Body.String())
	}
	futureBody := `{"config_id":"` + second.ConfigID + `","device_id":"` + second.DeviceID + `","applied_at":"2026-08-27T13:00:00Z"}`
	future := ack(futureBody, second.Generation)
	if future.Code != http.StatusBadRequest || !strings.Contains(future.Body.String(), "applied_at_in_future") {
		t.Fatalf("future ACK: %d %s", future.Code, future.Body.String())
	}
}

func TestOverlaySignedConfigAckRejectsUnknownAndSecretFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _, _ := newSignedConfigTestService(t)
	router, _, token := newAuthenticatedSyncHarness(t, WithOverlayProjectionService(service))
	for _, body := range []string{
		`{"config_id":"cfg_test","device_id":"dev_test","unknown":true}`,
		`{"config_id":"cfg_test","device_id":"dev_test","private_key":"forbidden"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/signed-config/1/ack", bytes.NewBufferString(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("payload %s status = %d: %s", body, recorder.Code, recorder.Body.String())
		}
	}
}
