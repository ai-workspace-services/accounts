package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
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
	for _, endpoint := range []string{"/api/agent-server/v1/nodes", "/api/agent/nodes", "/api/agent-server/v1/regional-pools"} {
		req := httptest.NewRequest(http.MethodGet, endpoint, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || rr.Body.String() != "[]" {
			t.Fatalf("%s: %d %s", endpoint, rr.Code, rr.Body.String())
		}
	}
}

func TestRegionalClosureReasonsAndMixedAvailability(t *testing.T) {
	now := time.Now().UTC()
	closed := false
	healthy := agentserver.StatusSnapshot{Agent: agentserver.Identity{ID: "sg-1"}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Region: "sg", Pool: "sg-main", EntryPoint: "sg.entry.example", Running: true}}}
	for _, reason := range []regionalClosureReason{regionalExplicitDisabled, regionalStale, regionalUnhealthy, regionalXrayNotRunning} {
		t.Run(string(reason), func(t *testing.T) {
			snapshot := healthy
			switch reason {
			case regionalExplicitDisabled:
				snapshot.Report.Xray.OpenToUsers = &closed
			case regionalStale:
				snapshot.UpdatedAt = now.Add(-regionalHeartbeatTTL - time.Second)
			case regionalUnhealthy:
				snapshot.Report.Healthy = false
			case regionalXrayNotRunning:
				snapshot.Report.Xray.Running = false
			}
			pools := registeredRegionalPools(stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{snapshot}}, now)
			if len(pools) != 1 || pools[0].OpenToUsers || !reflect.DeepEqual(pools[0].ClosedReasons, []regionalClosureReason{reason}) {
				t.Fatalf("closed pools=%#v", pools)
			}
			pools = registeredRegionalPools(stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{snapshot, healthy}}, now)
			if len(pools) != 1 || !pools[0].OpenToUsers || len(pools[0].ClosedReasons) != 0 || pools[0].PoolCount != 1 {
				t.Fatalf("mixed availability=%#v", pools)
			}
		})
	}
	failed := healthy
	failed.Report.Xray.OpenToUsers = &closed
	failed.UpdatedAt = now.Add(time.Minute) // future snapshots also fail freshness
	failed.Report.Healthy = false
	failed.Report.Xray.Running = false
	pools := registeredRegionalPools(stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{failed}}, now)
	want := []regionalClosureReason{regionalExplicitDisabled, regionalStale, regionalUnhealthy, regionalXrayNotRunning}
	if !reflect.DeepEqual(pools[0].ClosedReasons, want) {
		t.Fatalf("reasons=%v", pools[0].ClosedReasons)
	}
}

func TestRegionalPoolsKeepClosedMetadataAdminOnly(t *testing.T) {
	now := time.Now().UTC()
	st := store.NewMemoryStore()
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		user := &store.User{Name: role, Email: role + "@example.test", Role: role, Level: store.LevelUser, Active: true}
		if err := st.CreateUser(context.Background(), user); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(context.Background(), role+"-token", user.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	reader := stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{
		{Agent: agentserver.Identity{ID: "jp-1"}, UpdatedAt: now, Report: agentproto.StatusReport{Healthy: true, Xray: agentproto.XrayStatus{Region: "jp", EntryPoint: "jp.entry.example", Running: true}}},
		{Agent: agentserver.Identity{ID: "sg-1"}, UpdatedAt: now, Report: agentproto.StatusReport{Healthy: false, Xray: agentproto.XrayStatus{Region: "sg", EntryPoint: "sg.entry.example", Running: true}}},
	}}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentStatusReader(reader))
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/regional-pools", nil)
		req.Header.Set("Authorization", "Bearer "+role+"-token")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var pools []regionalPool
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", role, rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &pools); err != nil {
			t.Fatal(err)
		}
		if role == store.RoleUser && (len(pools) != 1 || pools[0].Code != "jp") {
			t.Fatalf("ordinary user saw closed pool: %#v", pools)
		}
		if role == store.RoleAdmin && (len(pools) != 2 || pools[1].ClosedReasons[0] != regionalUnhealthy) {
			t.Fatalf("admin diagnostics: %#v", pools)
		}
	}
}
