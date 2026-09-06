package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParseProxyNodeHosts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		configured string
		registered []string
		want       []string
	}{
		{name: "no proxies", want: []string{}},
		{name: "invalid entries", configured: "*", registered: []string{"", "*"}, want: []string{}},
		{name: "registered proxy", registered: []string{"jp-proxy.svc.plus"}, want: []string{"jp-proxy.svc.plus"}},
		{name: "configured and registered proxies", configured: "https://jp-proxy.svc.plus:443;us-proxy.svc.plus", registered: []string{"jp-proxy.svc.plus", "hk-proxy.svc.plus"}, want: []string{"jp-proxy.svc.plus", "us-proxy.svc.plus", "hk-proxy.svc.plus"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XRAY_PROXY_NODES", tt.configured)
			if got := parseProxyNodeHosts(tt.registered); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("nodes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentEndpointsReturnEmptyWithoutProxyNodes(t *testing.T) {
	t.Setenv("XRAY_PROXY_NODES", "")
	router, _, token := newAuthenticatedSyncHarness(t, WithServerPublicURL("https://accounts.svc.plus"))
	for _, endpoint := range []string{"/api/auth/sync/config?since_version=0", "/api/agent/nodes", "/api/agent-server/v1/nodes"} {
		t.Run(endpoint, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, endpoint, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}
			var payload any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			lists := []any{payload}
			if sync, ok := payload.(map[string]any); ok {
				lists = []any{sync["nodes"], sync["profiles"]}
			}
			for _, list := range lists {
				if nodes, ok := list.([]any); !ok || len(nodes) != 0 {
					t.Fatalf("expected empty array, got %#v", list)
				}
			}
		})
	}
}
