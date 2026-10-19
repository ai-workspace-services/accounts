package api

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
)

func TestUserNodeDiscoverySharedAndDedicatedMatrix(t *testing.T) {
	now := time.Now().UTC()
	reader := stubAgentStatusReader{}
	for _, node := range []struct{ id, class, team, pool string }{
		{"free", "free", "", "arbitrary"},
		{"plus", "plus", "", "arbitrary"},
		{"shared", "shared", "", "jp-plus"},
		{"team-a", "team", "a", "jp-shared"},
		{"team-b", "team", "b", "jp-shared"},
	} {
		groups := []string{"resource:" + node.class}
		if node.team != "" {
			groups = []string{"team:" + node.team}
		}
		reader.statuses = append(reader.statuses, agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: node.id, Groups: groups}, UpdatedAt: now,
			Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", Pool: node.pool, EntryPoint: node.id + ".example"}},
		})
	}
	for _, tc := range []struct {
		name, group string
		access      networkAccess
		want        []string
	}{
		{"registered free", "", networkAccess{Mode: "personal", Allowed: true}, []string{"free.example", "shared.example"}},
		{"free", store.MonthlyFreeQuotaLimitGroup, networkAccess{Mode: "personal", Allowed: true}, []string{"free.example", "shared.example"}},
		{"plus", store.MonthlyPlusQuotaLimitGroup, networkAccess{Mode: "personal", Allowed: true}, []string{"free.example", "plus.example", "shared.example"}},
		{"team a", "", networkAccess{Mode: "dedicated", TeamID: "a", Allowed: true}, []string{"team-a.example"}},
		{"team b", "", networkAccess{Mode: "dedicated", TeamID: "b", Allowed: true}, []string{"team-b.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pools := regionalPoolsForUser(reader, tc.access, &store.User{Groups: []string{tc.group}}, "", now)
			var got []string
			for _, pool := range pools {
				got = append(got, pool.Entry)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("nodes=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestPoolXHTTPPathUsesEntitlementRoute(t *testing.T) {
	t.Setenv("XRAY_POOL_ROUTE_PREFIX", "/pool")
	for _, tc := range []struct {
		class string
		want  string
	}{
		{class: agentResourceFree, want: "/pool/free/split"},
		{class: agentResourcePlus, want: "/pool/plus/split"},
		{class: agentResourcePro, want: "/pool/pro/split"},
		{class: agentResourceTeam, want: "/pool/team/split"},
	} {
		if got := poolXHTTPPath("/split", tc.class); got != tc.want {
			t.Fatalf("poolXHTTPPath(%q) = %q, want %q", tc.class, got, tc.want)
		}
	}
	t.Setenv("XRAY_POOL_ROUTE_PREFIX", "none")
	if got := poolXHTTPPath("split", agentResourceFree); got != "/split" {
		t.Fatalf("empty route prefix = %q, want /split", got)
	}
}

func TestRegionalPoolsForUserSelectsReportedResourceClass(t *testing.T) {
	now := time.Now().UTC()
	status := func(id, pool string, groups []string) agentserver.StatusSnapshot {
		return agentserver.StatusSnapshot{
			Agent:     agentserver.Identity{ID: id, Groups: groups},
			UpdatedAt: now,
			Report:    agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", Pool: pool, EntryPoint: "jp-xconnect.onwalk.net"}},
		}
	}
	reader := stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{
		status("free-1", "jp-free", []string{"resource:free"}),
		status("plus-1", "jp-plus", []string{"resource:plus"}),
		status("pro-1", "jp-pro", []string{"resource:pro"}),
	}}
	free := &store.User{Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	plus := &store.User{Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	pro := &store.User{Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	personal := networkAccess{Mode: "personal", Allowed: true}
	freePools := regionalPoolsForUser(reader, personal, free, "free", now)
	plusPools := regionalPoolsForUser(reader, personal, plus, "plus", now)
	proPools := regionalPoolsForUser(reader, personal, pro, "pro", now)
	if len(freePools) != 1 || freePools[0].ResourceClass != agentResourceFree || freePools[0].Entry != "jp-xconnect.onwalk.net" {
		t.Fatalf("free pools = %#v", freePools)
	}
	if len(plusPools) != 2 {
		t.Fatalf("plus pools = %#v", plusPools)
	}
	if len(proPools) != 1 || proPools[0].ResourceClass != agentResourcePro || proPools[0].Entry != "jp-xconnect.onwalk.net" {
		t.Fatalf("pro pools = %#v", proPools)
	}
}

func TestPlusNodesIncludeFreePoolWithFreeTransportRoute(t *testing.T) {
	t.Setenv("XRAY_POOL_ROUTE_PREFIX", "/pool")
	ctx := context.Background()
	st := store.NewMemoryStore()
	now := time.Now().UTC()
	user := &store.User{Name: "plus", Email: "plus@example.test", Active: true, EmailVerified: true, ProxyUUID: "proxy-plus", Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "plus-session", user.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reader := stubAgentStatusReader{}
	for _, class := range []string{"free", "shared", "plus"} {
		reader.statuses = append(reader.statuses, agentserver.StatusSnapshot{Agent: agentserver.Identity{ID: class, Groups: []string{"resource:" + class}}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: class + ".example"}}})
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentStatusReader(reader))
	req := httptest.NewRequest("GET", "/api/agent-server/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer plus-session")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var nodes []VlessNode
	if err := json.Unmarshal(rr.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes=%+v", nodes)
	}
	for _, node := range nodes {
		want := "/pool/plus/split"
		if node.Address == "free.example" {
			want = "/pool/free/split"
			if node.ResourceClass != "free" {
				t.Fatalf("free route class=%s", node.ResourceClass)
			}
		}
		if node.Path != want {
			t.Fatalf("node=%s path=%s want=%s", node.Address, node.Path, want)
		}
	}
}
