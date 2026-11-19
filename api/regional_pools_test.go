package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/routingpolicy"
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
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "shared", Token: "test-agent-token", Groups: []string{"resource:free"}}}})
	if err != nil {
		t.Fatal(err)
	}
	router, _, token := newAuthenticatedSyncHarness(t, WithAgentRegistry(registry), WithAgentStatusReader(registry))
	report := []byte(`{"agentId":"runtime-de-1","role":"agent-proxy","healthy":true,"xray":{"running":true,"region":"de-fra","pool":"pool-a-free","entryPoint":"de.entry.example","openToUsers":true}}`)
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

func TestRegionalPoolsRequireAgentEntrypoint(t *testing.T) {
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
		user := &store.User{Name: role, Email: role + "@example.test", Role: role, Level: store.LevelUser, Active: true, EmailVerified: true}
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

func TestRegionalPoolRoutingMetadataAndCustomIsolation(t *testing.T) {
	now := time.Now().UTC()
	reader := stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{
		{
			Agent:     agentserver.Identity{ID: "jp-media", Name: "JP-Media", Groups: []string{"resource:shared"}},
			UpdatedAt: now,
			Report:    agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: "jp-xconnect-01.svc.plus"}},
		},
		{
			Agent:     agentserver.Identity{ID: "jp-pro", Name: "JP-Pro", Groups: []string{"resource:pro"}},
			UpdatedAt: now,
			Report:    agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: "jp-xconnect-02.svc.plus"}},
		},
		{
			Agent:     agentserver.Identity{ID: "us-custom", Name: "US-Custom", Groups: []string{"resource:custom"}},
			UpdatedAt: now,
			Report:    agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "us", EntryPoint: "us-xconnect-los-angeles-custom-01.svc.plus"}},
		},
	}}

	all := registeredRegionalPools(reader, now)
	if len(all) != 3 {
		t.Fatalf("expected 3 registered pools, got %d: %#v", len(all), all)
	}
	byEntry := map[string]regionalPool{}
	for _, p := range all {
		byEntry[p.Entry] = p
	}
	if p := byEntry["jp-xconnect-01.svc.plus"]; p.RoutingPool != routingpolicy.RoutingPoolMediaPriority || !p.PublicResource || p.MediaOnProAction != routingpolicy.MediaOnProActionAllow || len(p.SubPoolTags) == 0 {
		t.Fatalf("Media-Pool metadata mismatch: %+v", p)
	}
	if p := byEntry["jp-xconnect-02.svc.plus"]; p.RoutingPool != routingpolicy.RoutingPoolProEC2 || !p.PublicResource || p.MediaOnProAction != routingpolicy.MediaOnProActionAllow {
		t.Fatalf("Pro-EC2-Pool metadata mismatch: %+v", p)
	}
	if p := byEntry["us-xconnect-los-angeles-custom-01.svc.plus"]; p.RoutingPool != routingpolicy.RoutingPoolCustomDedicated || p.PublicResource {
		t.Fatalf("Custom-Dedicated metadata mismatch (must have PublicResource=false): %+v", p)
	}

	pub := publicRegionalPools(reader, now)
	if len(pub) != 2 {
		t.Fatalf("publicRegionalPools must exclude Custom-Dedicated, got %#v", pub)
	}
	for _, p := range pub {
		if p.RoutingPool == routingpolicy.RoutingPoolCustomDedicated || p.Entry == "us-xconnect-los-angeles-custom-01.svc.plus" {
			t.Fatalf("Custom-Dedicated leaked into publicRegionalPools: %+v", p)
		}
	}

	personal := networkAccess{Mode: "personal", Allowed: true}
	freeUser := &store.User{Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	proUser := &store.User{Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	customUser := &store.User{Groups: []string{"segment:custom"}}

	for _, pool := range regionalPoolsForUser(reader, personal, freeUser, "free", now) {
		if pool.RoutingPool == routingpolicy.RoutingPoolCustomDedicated || pool.RoutingPool == routingpolicy.RoutingPoolProEC2 {
			t.Fatalf("free user leaked restricted pool: %+v", pool)
		}
	}
	for _, pool := range regionalPoolsForUser(reader, personal, proUser, "pro", now) {
		if pool.RoutingPool == routingpolicy.RoutingPoolCustomDedicated {
			t.Fatalf("pro user leaked Custom-Dedicated pool: %+v", pool)
		}
	}
	customPools := regionalPoolsForUser(reader, personal, customUser, "custom", now)
	foundCustom := false
	for _, pool := range customPools {
		if pool.RoutingPool == routingpolicy.RoutingPoolCustomDedicated && pool.Entry == "us-xconnect-los-angeles-custom-01.svc.plus" {
			foundCustom = true
		}
	}
	if !foundCustom {
		t.Fatalf("whitelisted custom user missing Custom-Dedicated pool: %#v", customPools)
	}
}

func TestSubscriptionAndSyncConfigRoutesEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	now := time.Now().UTC()
	st := store.NewMemoryStore()

	proUser := &store.User{Name: "pro", Email: "pro-sub@example.invalid", Active: true, EmailVerified: true, ProxyUUID: "11111111-1111-1111-1111-111111111111", Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	customUser := &store.User{Name: "custom", Email: "custom-sub@example.invalid", Active: true, EmailVerified: true, ProxyUUID: "22222222-2222-2222-2222-222222222222", Groups: []string{"segment:custom"}}
	for _, u := range []*store.User{proUser, customUser} {
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, "tok-"+u.Name, u.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: proUser.ID, PackageName: "pro", IncludedQuotaBytes: 50 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: proUser.ID, RemainingIncludedQuota: 50 << 30, SuspendState: "active", ProxyAccessState: "active"}); err != nil {
		t.Fatal(err)
	}

	reader := stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{
		{Agent: agentserver.Identity{ID: "jp-media", Name: "JP-Media", Groups: []string{"resource:shared"}}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: "jp-xconnect-01.svc.plus"}}},
		{Agent: agentserver.Identity{ID: "jp-pro", Name: "JP-Pro", Groups: []string{"resource:pro"}}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: "jp-xconnect-02.svc.plus"}}},
		{Agent: agentserver.Identity{ID: "us-custom", Name: "US-Custom", Groups: []string{"resource:custom"}}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "us", EntryPoint: "us-xconnect-los-angeles-custom-01.svc.plus"}}},
	}}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentStatusReader(reader), WithXrayConfigRenderer(func(*store.User) (string, string, []string, error) {
		return `{"outbounds":[{"tag":"proxy"}]}`, "digest-1", nil, nil
	}))

	// 1. /api/agent-server/v1/nodes populates RoutingPool and PricingGroup with config-agnostic vless:// URIs
	req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer tok-pro")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nodes: %d %s", rr.Code, rr.Body.String())
	}
	var nodes []VlessNode
	if err := json.Unmarshal(rr.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes (Media-Pool + Pro-EC2-Pool), got %d: %+v", len(nodes), nodes)
	}
	for _, n := range nodes {
		if n.RoutingPool == "" || n.PricingGroup == "" {
			t.Fatalf("node missing RoutingPool or PricingGroup: %+v", n)
		}
		if n.RoutingPool == routingpolicy.RoutingPoolCustomDedicated {
			t.Fatalf("pro user saw Custom-Dedicated node: %+v", n)
		}
		parsedURI, err := url.Parse(n.URISchemeXHTTP)
		if err != nil {
			t.Fatalf("invalid vless URI %q: %v", n.URISchemeXHTTP, err)
		}
		if parsedURI.Query().Has("routing_pool") || parsedURI.Query().Has("pricing_group") {
			t.Fatalf("vless URI must remain config-agnostic: %q", n.URISchemeXHTTP)
		}
	}

	// 2. /api/auth/sync/config populates three-tier "routes"
	req = httptest.NewRequest(http.MethodGet, "/api/auth/sync/config?since_version=0", nil)
	req.Header.Set("Authorization", "Bearer tok-pro")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync/config: %d %s", rr.Code, rr.Body.String())
	}
	var syncPayload struct {
		Routes []struct {
			Name            string `json:"name"`
			Target          string `json:"target"`
			EffectiveTarget string `json:"effective_target"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &syncPayload); err != nil {
		t.Fatal(err)
	}
	if len(syncPayload.Routes) != 4 || syncPayload.Routes[0].Name != "direct_cn_private" || syncPayload.Routes[2].EffectiveTarget != routingpolicy.RoutingPoolProEC2 || syncPayload.Routes[3].EffectiveTarget != routingpolicy.RoutingPoolMediaPriority {
		t.Fatalf("unexpected sync/config routes: %+v", syncPayload.Routes)
	}

	// 3. /api/agent-server/v1/subscription & /api/auth/sync/subscription/:format
	for _, endpoint := range []string{
		"/api/agent-server/v1/subscription?format=clash",
		"/api/auth/sync/subscription/singbox",
		"/api/auth/sync/subscription/xray",
		"/api/auth/sync/subscription/one",
		"/api/auth/sync/subscription/vless",
	} {
		req := httptest.NewRequest(http.MethodGet, endpoint, nil)
		req.Header.Set("Authorization", "Bearer tok-pro")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", endpoint, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "us-xconnect-los-angeles-custom-01.svc.plus") {
			t.Fatalf("%s leaked Custom-Dedicated node to pro user", endpoint)
		}
	}

	// Whitelisted custom user receives Custom-Dedicated in Clash subscription
	req = httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/subscription?format=clash&token=tok-custom", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "us-xconnect-los-angeles-custom-01.svc.plus") || !strings.Contains(rr.Body.String(), routingpolicy.RoutingPoolCustomDedicated) {
		t.Fatalf("custom user clash subscription failed: %d %s", rr.Code, rr.Body.String())
	}

	// Unsupported format returns 400
	req = httptest.NewRequest(http.MethodGet, "/api/auth/sync/subscription/invalid-fmt", nil)
	req.Header.Set("Authorization", "Bearer tok-pro")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid format expected 400, got %d", rr.Code)
	}
}
