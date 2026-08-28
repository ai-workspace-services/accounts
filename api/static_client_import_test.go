package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const staticImportCanonicalGolden = `{"schema_version":1,"kind":"xconnect.accounts.static-client-import","network_id":"network-private","owner_user_id":"11111111-1111-4111-8111-111111111111","source":{"kind":"ansible-group-vars","variable":"xworkmate_bridge_distributed_vpn_clients","baseline_sha256":"42001fd0d7734955c2f0d33d0f4583f96523d09dca36b3e149cec78eff795e69"},"devices":[{"device_id":"device-alpha","wireguard_public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","addresses":["10.77.0.10/32"],"tags":["migration:static-group-vars","team:platform"],"attachments":["gateway-a","gateway-b"]},{"device_id":"device-beta","wireguard_public_key":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=","addresses":["10.77.0.11/32"],"tags":["migration:static-group-vars"],"attachments":["gateway-b"]}]}`

func TestStaticImportMatchesPlaybooksGoldenAndIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test")
	st := store.NewMemoryStore()
	if err := st.CreateUser(t.Context(), &store.User{ID: "11111111-1111-4111-8111-111111111111", Name: "import-owner", Email: "import@example.test"}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st))
	post := func(body, key, contentType string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/imports/static-clients", bytes.NewBufferString(body))
		request.Header.Set("X-Service-Token", "internal-test")
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	const key = "sha256-911905502b4aa02c4c82b16e200f5f13caebd534898566b8b87384d972ed1fd2"
	first := post(staticImportCanonicalGolden, key, staticImportMediaType)
	if first.Code != http.StatusOK {
		t.Fatalf("import=%d %s", first.Code, first.Body.String())
	}
	retry := post(staticImportCanonicalGolden, key, staticImportMediaType)
	if retry.Code != http.StatusOK || retry.Body.String() != first.Body.String() {
		t.Fatalf("receipt changed: first=%s retry=%s", first.Body.String(), retry.Body.String())
	}
	devices, err := st.ListOverlayProjectionDevicesByNetwork(t.Context(), "network-private")
	if err != nil || len(devices) != 2 {
		t.Fatalf("devices=%#v err=%v", devices, err)
	}
	wrong := post(staticImportCanonicalGolden, "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", staticImportMediaType)
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong idempotency=%d %s", wrong.Code, wrong.Body.String())
	}
	pretty := post("\n"+staticImportCanonicalGolden, key, staticImportMediaType)
	if pretty.Code != http.StatusBadRequest {
		t.Fatalf("noncanonical body=%d %s", pretty.Code, pretty.Body.String())
	}
	wrongMedia := post(staticImportCanonicalGolden, key, "application/json")
	if wrongMedia.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong media=%d", wrongMedia.Code)
	}
	nodeBearer := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/imports/static-clients", bytes.NewBufferString(staticImportCanonicalGolden))
	nodeBearer.Header.Set("Authorization", "Bearer xgn_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ")
	nodeBearer.Header.Set("Content-Type", staticImportMediaType)
	nodeBearer.Header.Set("Idempotency-Key", key)
	nodeResponse := httptest.NewRecorder()
	router.ServeHTTP(nodeResponse, nodeBearer)
	if nodeResponse.Code != http.StatusUnauthorized {
		t.Fatalf("node bearer escaped management boundary: %d", nodeResponse.Code)
	}
}
