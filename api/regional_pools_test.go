package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
)

func TestRegionalPoolsAggregateReportsAndAvailability(t *testing.T) {
	now := time.Now().UTC()
	closed := false
	snapshot := func(id, pool string) agentserver.StatusSnapshot {
		return agentserver.StatusSnapshot{Agent: agentserver.Identity{ID: id}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "de-fra", Pool: pool, EntryPoint: "https://DE.entry.example:443"}}}
	}
	a, b, c := snapshot("runtime-a", "pool-a"), snapshot("runtime-b", "pool-a"), snapshot("runtime-c", "pool-b")
	pools := registeredRegionalPools(stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{a, b, c, {Agent: agentserver.Identity{ID: "configured.example"}}}}, now)
	if len(pools) != 1 || pools[0].Entry != "de.entry.example" || pools[0].Code != "de-fra" || pools[0].PoolCount != 2 || !pools[0].OpenToUsers {
		t.Fatalf("pools = %#v", pools)
	}
	a.Report.Xray.OpenToUsers = &closed
	b.Report.Healthy = false
	c.UpdatedAt = now.Add(-regionalHeartbeatTTL - time.Second)
	pools = registeredRegionalPools(stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{a, b, c}}, now)
	if len(pools) != 1 || pools[0].OpenToUsers {
		t.Fatalf("closed/unhealthy/stale entries = %#v", pools)
	}
	for _, role := range []string{"gateway", "one"} {
		a.Report.Role = role
		if got := registeredRegionalPools(stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{a}}, now); len(got) != 0 {
			t.Fatalf("%s exposed: %#v", role, got)
		}
	}
}

func TestRegionalEntryHostRejectsRuntimeIDsAndAddresses(t *testing.T) {
	for _, raw := range []string{"", "*", "runtime-node-1", "127.0.0.1", "https://[::1]", "https://user:pass@entry.example", "bad_host.example"} {
		if got := regionalEntryHost(raw); got != "" {
			t.Errorf("regionalEntryHost(%q) = %q", raw, got)
		}
	}
}

func TestRegionalRegistrationEndpointsUseReportedMetadata(t *testing.T) {
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "shared", Token: "test-agent-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	router, _, token := newAuthenticatedSyncHarness(t, WithAgentRegistry(registry), WithAgentStatusReader(registry))
	report := []byte(`{"agentId":"runtime-de-1","role":"agent-proxy","healthy":true,"xray":{"running":true,"region":"de-fra","pool":"pool-a","entryPoint":"de.entry.example","openToUsers":true}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent-server/v1/status", bytes.NewReader(report))
	req.Header.Set("Authorization", "Bearer test-agent-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("registration: %d %s", rr.Code, rr.Body.String())
	}
	get := func(path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	rr = get("/api/agent-server/v1/regional-pools", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("pools: %d %s", rr.Code, rr.Body.String())
	}
	var pools []regionalPool
	if err := json.Unmarshal(rr.Body.Bytes(), &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].Entry != "de.entry.example" || pools[0].PoolCount != 1 {
		t.Fatalf("pools=%#v", pools)
	}
	rr = get("/api/agent-server/v1/nodes", token)
	var nodes []VlessNode
	if err := json.Unmarshal(rr.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Address != "de.entry.example" || nodes[0].Region != "de-fra" || !nodes[0].OpenToUsers {
		t.Fatalf("nodes = %#v", nodes)
	}
	if rr = get("/api/agent-server/v1/regional-pools", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/agent-server/v1/status", bytes.NewReader(bytes.Replace(report, []byte(`"openToUsers":true`), []byte(`"openToUsers":false`), 1)))
	req.Header.Set("Authorization", "Bearer test-agent-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("close report: %d %s", rr.Code, rr.Body.String())
	}

	rr = get("/api/agent-server/v1/nodes", token)
	if rr.Code != http.StatusOK || rr.Body.String() != "[]" {
		t.Fatalf("closed selector: %d %s", rr.Code, rr.Body.String())
	}
}

func TestRegionalPoolsIgnoreConfiguredProxyList(t *testing.T) {
	t.Setenv("XRAY_PROXY_NODES", "fixed.entry.example")
	router, _, token := newAuthenticatedSyncHarness(t, WithAgentStatusReader(stubAgentStatusReader{}))
	for _, endpoint := range []string{"/api/agent-server/v1/nodes", "/api/agent-server/v1/regional-pools"} {
		req := httptest.NewRequest(http.MethodGet, endpoint, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || rr.Body.String() != "[]" {
			t.Fatalf("%s: %d %s", endpoint, rr.Code, rr.Body.String())
		}
	}
}
