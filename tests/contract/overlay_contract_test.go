package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"account/internal/overlay/domain"
	"gopkg.in/yaml.v3"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func readFile(t *testing.T, relativePath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), relativePath))
	if err != nil {
		t.Fatalf("read %s: %v", relativePath, err)
	}
	return raw
}

func TestOverlayOpenAPIExposesVersionedBaseline(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(readFile(t, "api/openapi/overlay-v1.yaml"), &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	if got := document["openapi"]; got != "3.1.0" {
		t.Fatalf("expected OpenAPI 3.1.0, got %#v", got)
	}
	info, ok := document["info"].(map[string]any)
	if !ok || info["x-xconnect-product-family"] != "XConnect-One" {
		t.Fatalf("missing XConnect-One product family marker: %#v", info)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("missing OpenAPI paths: %#v", document["paths"])
	}
	for _, path := range []string{
		"/api/overlay/v1/networks",
		"/api/overlay/v1/devices",
		"/api/overlay/v1/devices/register",
		"/api/overlay/v1/config",
		"/api/overlay/v1/config/ack",
		"/api/internal/overlay/v1/nodes/heartbeat",
	} {
		if _, exists := paths[path]; !exists {
			t.Errorf("OpenAPI missing %s", path)
		}
	}
}

func TestOverlaySchemasLockProxyCoreToXray(t *testing.T) {
	for _, relativePath := range []string{
		"api/schemas/overlay/signed-config-v1.schema.json",
		"api/schemas/overlay/gateway-snapshot-v1.schema.json",
	} {
		var schema map[string]any
		if err := json.Unmarshal(readFile(t, relativePath), &schema); err != nil {
			t.Fatalf("parse %s: %v", relativePath, err)
		}
		properties := schema["properties"].(map[string]any)
		transport := properties["transport"].(map[string]any)
		transportProperties := transport["properties"].(map[string]any)
		proxyCore := transportProperties["proxy_core"].(map[string]any)
		if got := proxyCore["const"]; got != domain.ProxyCoreXray {
			t.Errorf("%s proxy_core const = %#v, want %q", relativePath, got, domain.ProxyCoreXray)
		}
	}
}

func TestSignedConfigFixtureMatchesDomainContract(t *testing.T) {
	var config domain.SignedConfig
	if err := json.Unmarshal(readFile(t, "tests/fixtures/overlay/signed-config-v1.json"), &config); err != nil {
		t.Fatalf("decode signed config fixture: %v", err)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("signed config fixture invalid: %v", err)
	}
}

func TestGatewaySnapshotFixtureMatchesDomainContract(t *testing.T) {
	var snapshot domain.GatewaySnapshot
	if err := json.Unmarshal(readFile(t, "tests/fixtures/overlay/gateway-snapshot-v1.json"), &snapshot); err != nil {
		t.Fatalf("decode gateway snapshot fixture: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("gateway snapshot fixture invalid: %v", err)
	}
}
