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
		"/api/overlay/v1/signed-config",
		"/api/overlay/v1/signing-keys",
		"/api/overlay/v1/signed-config/{generation}/ack",
		"/api/internal/overlay/v1/nodes/heartbeat",
	} {
		if _, exists := paths[path]; !exists {
			t.Errorf("OpenAPI missing %s", path)
		}
	}
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	refs := map[string]string{
		"SignedConfig":    "../schemas/overlay/signed-config.schema.json",
		"GatewaySnapshot": "../schemas/overlay/gateway-snapshot.schema.json",
	}
	for name, want := range refs {
		schema := schemas[name].(map[string]any)
		if got := schema["$ref"]; got != want {
			t.Errorf("OpenAPI %s ref = %#v, want %q", name, got, want)
		}
	}
}

func TestOverlaySchemasLockProxyCoreToXray(t *testing.T) {
	tests := []struct {
		path string
		id   string
	}{
		{
			path: "api/schemas/overlay/signed-config.schema.json",
			id:   "https://accounts.svc.plus/schemas/overlay/v1/signed-config.schema.json",
		},
		{
			path: "api/schemas/overlay/gateway-snapshot.schema.json",
			id:   "https://accounts.svc.plus/schemas/overlay/v1/gateway-snapshot.schema.json",
		},
	}
	for _, test := range tests {
		var schema map[string]any
		if err := json.Unmarshal(readFile(t, test.path), &schema); err != nil {
			t.Fatalf("parse %s: %v", test.path, err)
		}
		if got := schema["$id"]; got != test.id {
			t.Errorf("%s $id = %#v, want %q", test.path, got, test.id)
		}
		properties := schema["properties"].(map[string]any)
		proxyCore := properties["proxy_core"].(map[string]any)
		if got := proxyCore["const"]; got != domain.ProxyCoreXray {
			t.Errorf("%s proxy_core const = %#v, want %q", test.path, got, domain.ProxyCoreXray)
		}
	}
}

func TestGatewaySchemaRequiresGenerationAndEmptyPeerSafety(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(readFile(t, "api/schemas/overlay/gateway-snapshot.schema.json"), &schema); err != nil {
		t.Fatalf("parse gateway schema: %v", err)
	}
	requiredValues := schema["required"].([]any)
	required := make(map[string]bool, len(requiredValues))
	for _, value := range requiredValues {
		required[value.(string)] = true
	}
	for _, field := range []string{
		"expected_previous_generation",
		"safety",
	} {
		if !required[field] {
			t.Errorf("gateway schema does not require %s", field)
		}
	}
}

func TestSignedConfigFixtureMatchesDomainContract(t *testing.T) {
	if _, err := domain.DecodeSignedConfig(readFile(t, "tests/fixtures/overlay/signed-config.json")); err != nil {
		t.Fatalf("signed config fixture invalid: %v", err)
	}
}

func TestGatewaySnapshotFixtureMatchesDomainContract(t *testing.T) {
	if _, err := domain.DecodeGatewaySnapshot(readFile(t, "tests/fixtures/overlay/gateway-snapshot.json")); err != nil {
		t.Fatalf("gateway snapshot fixture invalid: %v", err)
	}
}
