package api

import (
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
		{"plus", store.MonthlyPlusQuotaLimitGroup, networkAccess{Mode: "personal", Allowed: true}, []string{"plus.example", "shared.example"}},
		{"team a", "", networkAccess{Mode: "dedicated", TeamID: "a", Allowed: true}, []string{"team-a.example"}},
		{"team b", "", networkAccess{Mode: "dedicated", TeamID: "b", Allowed: true}, []string{"team-b.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pools := regionalPoolsForUser(reader, tc.access, &store.User{Groups: []string{tc.group}}, now)
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
	}}
	free := &store.User{Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	plus := &store.User{Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	personal := networkAccess{Mode: "personal", Allowed: true}
	freePools := regionalPoolsForUser(reader, personal, free, now)
	plusPools := regionalPoolsForUser(reader, personal, plus, now)
	if len(freePools) != 1 || freePools[0].ResourceClass != agentResourceFree || freePools[0].Entry != "jp-xconnect.onwalk.net" {
		t.Fatalf("free pools = %#v", freePools)
	}
	if len(plusPools) != 1 || plusPools[0].ResourceClass != agentResourcePlus || plusPools[0].Entry != "jp-xconnect.onwalk.net" {
		t.Fatalf("plus pools = %#v", plusPools)
	}
}
