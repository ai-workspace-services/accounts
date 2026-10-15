package api

import (
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
)

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
