package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"account/internal/agentserver"

	"github.com/gin-gonic/gin"
)

func TestAgentManagedConfigRoute(t *testing.T) {
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: "agent-a", NodeID: "node-a", Token: "token-a", Groups: []string{"resource:free"}},
		{ID: "agent-b", NodeID: "node-b", Token: "token-b", Groups: []string{"resource:shared"}},
		{ID: "*", Token: "legacy"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "managed.json")
	document := []byte(`{
	  "schemaVersion": 1,
	  "revision": "0123456789012345678901234567890123456789",
	  "templates": {"free-shared":{"pool":"default","statsEnabled":true},"plus":{},"pro":{},"custom":{}},
	  "nodeOverrides":{"node-a":{"pool":"private-a","statsEnabled":false},"node-b":{"pool":"private-b"}}
	}`)
	if err := os.WriteFile(path, document, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, token string
		status            int
	}{
		{"disabled", "", "token-a", http.StatusNotFound},
		{"missing credentials", path, "", http.StatusUnauthorized},
		{"legacy credentials", path, "legacy", http.StatusForbidden},
		{"bound credentials", path, "token-a", http.StatusOK},
		{"missing file", path + ".missing", "token-a", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			RegisterRoutes(router, WithAgentRegistry(registry), WithAgentManagedConfigPath(tc.file))
			req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/managed-config?nodeId=node-b", nil)
			req.Header.Set("X-Agent-ID", "agent-b")
			req.Header.Set("Authorization", tc.token)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", res.Code, tc.status, res.Body.String())
			}
			if tc.status == http.StatusOK {
				var got agentManagedSnapshot
				if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.NodeID != "node-a" || got.ResourceClass != "free" || got.Template != "free-shared" || got.Agent.Pool != "private-a" || got.Agent.StatsEnabled == nil || *got.Agent.StatsEnabled || got.Revision != "0123456789012345678901234567890123456789" {
					t.Fatalf("unexpected snapshot: %s", res.Body.String())
				}
			}
		})
	}
}

func TestResolveAgentManagedConfigRejectsProtectedFields(t *testing.T) {
	data := []byte(`{
	  "schemaVersion": 1,
	  "revision": "0123456789012345678901234567890123456789",
	  "templates": {"free-shared":{},"plus":{},"pro":{},"custom":{}},
	  "nodeOverrides":{"node-a":{"apiToken":"must-not-be-managed"}}
	}`)
	if _, err := resolveAgentManagedConfig(data, "node-a", "free", "free-shared"); err == nil {
		t.Fatal("protected field accepted")
	}
}
