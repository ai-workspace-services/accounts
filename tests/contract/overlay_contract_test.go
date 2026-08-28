package contract_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		"/api/overlay/v1/join-tokens",
		"/api/overlay/v1/join-tokens/{id}",
		"/api/overlay/v1/join-tokens/exchange",
		"/api/overlay/v1/enrollment/config",
		"/api/overlay/v1/enrollment/signed-config",
		"/api/overlay/v1/enrollment/config/ack",
		"/api/overlay/v1/enrollment/signed-config/{generation}/ack",
		"/api/overlay/v1/signed-config/{generation}/ack",
		"/api/internal/overlay/v1/nodes/heartbeat",
		"/api/internal/overlay/v1/nodes/{node_id}/snapshot",
		"/api/internal/overlay/v1/nodes/{node_id}/apply-result",
		"/api/internal/overlay/v1/nodes/{node_id}/credentials",
		"/api/internal/overlay/v1/nodes/{node_id}/credentials/{credential_id}",
		"/api/internal/overlay/v1/imports/static-clients",
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

func TestGatewayOpenAPILocksAuthenticationAndAuthorizedRuntimeBoundaries(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(readFile(t, "api/openapi/overlay-v1.yaml"), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	for _, path := range []string{"/api/internal/overlay/v1/nodes/heartbeat", "/api/internal/overlay/v1/nodes/{node_id}/snapshot", "/api/internal/overlay/v1/nodes/{node_id}/apply-result"} {
		methods := paths[path].(map[string]any)
		for _, operation := range methods {
			if !strings.Contains(fmt.Sprint(operation.(map[string]any)["security"]), "nodeBearer") {
				t.Fatalf("Agent path %s is not node-bound", path)
			}
		}
	}
	for _, path := range []string{"/api/internal/overlay/v1/nodes/{node_id}/credentials", "/api/internal/overlay/v1/imports/static-clients", "/api/internal/overlay/v1/reconcile-pending"} {
		operation := paths[path].(map[string]any)["post"].(map[string]any)
		if !strings.Contains(fmt.Sprint(operation["security"]), "serviceToken") {
			t.Fatalf("management path %s escaped service boundary", path)
		}
	}
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	heartbeat := schemas["GatewayHeartbeatRequest"].(map[string]any)["properties"].(map[string]any)
	if !strings.Contains(fmt.Sprint(heartbeat["mode"].(map[string]any)["enum"]), "shadow") || !strings.Contains(fmt.Sprint(heartbeat["mode"].(map[string]any)["enum"]), "apply") || heartbeat["proxy_core"].(map[string]any)["const"] != "xray" {
		t.Fatal("heartbeat modes or Xray-only boundary drifted")
	}
	apply := schemas["GatewayApplyResultRequest"].(map[string]any)["properties"].(map[string]any)
	results := fmt.Sprint(apply["result"].(map[string]any)["enum"])
	for _, want := range []string{"applied", "apply_rejected", "apply_failed_rolled_back", "apply_failed_rollback_failed"} {
		if !strings.Contains(results, want) {
			t.Fatalf("apply result enum missing %s: %s", want, results)
		}
	}
	credential := schemas["CreateNodeCredentialResponse"].(map[string]any)["properties"].(map[string]any)["credential"].(map[string]any)["properties"].(map[string]any)["bearer_token"].(map[string]any)
	if credential["writeOnly"] != true {
		t.Fatal("node raw credential is not writeOnly")
	}
}

func TestOverlayOpenAPILocksJoinAndEnrollmentSecurityBoundaries(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(readFile(t, "api/openapi/overlay-v1.yaml"), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	exchange := paths["/api/overlay/v1/join-tokens/exchange"].(map[string]any)["post"].(map[string]any)
	security, ok := exchange["security"].([]any)
	if !ok || len(security) != 0 {
		t.Fatalf("join exchange must be outside account auth middleware: %#v", exchange["security"])
	}
	enrollment := paths["/api/overlay/v1/enrollment/signed-config"].(map[string]any)["get"].(map[string]any)
	if !strings.Contains(fmt.Sprint(enrollment["security"]), "enrollmentBearer") {
		t.Fatalf("enrollment endpoint missing restricted bearer: %#v", enrollment["security"])
	}
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	exchangeRequest := schemas["ExchangeJoinTokenRequest"].(map[string]any)["properties"].(map[string]any)
	if exchangeRequest["join_token"].(map[string]any)["writeOnly"] != true {
		t.Fatal("join secret must be writeOnly")
	}
	exchangeResponse := schemas["ExchangeJoinTokenResponse"].(map[string]any)["properties"].(map[string]any)
	if exchangeResponse["enrollment_token"].(map[string]any)["writeOnly"] != true {
		t.Fatal("enrollment secret must be writeOnly")
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
