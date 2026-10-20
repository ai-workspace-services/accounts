package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestPublicTierAndTeamPermissionMatrix(t *testing.T) {
	now := time.Now().UTC()
	reader := stubAgentStatusReader{}
	for _, class := range []string{"shared", "free", "plus", "pro"} {
		reader.statuses = append(reader.statuses, agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: class, Groups: []string{"resource:" + class}}, UpdatedAt: now,
			Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: class + ".example"}},
		})
	}
	for _, team := range []string{"alpha", "beta"} {
		reader.statuses = append(reader.statuses, agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: team, Groups: []string{"team:" + team}}, UpdatedAt: now,
			Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: team + ".example"}},
		})
	}
	for _, tc := range []struct {
		name, pkg, group, team string
		want                   []string
	}{
		{"free", "free", store.MonthlyFreeQuotaLimitGroup, "", []string{"free.example", "shared.example"}},
		{"plus", "plus", store.MonthlyPlusQuotaLimitGroup, "", []string{"free.example", "plus.example", "shared.example"}},
		{"pro", "pro", store.MonthlyPlusQuotaLimitGroup, "", []string{"free.example", "plus.example", "pro.example", "shared.example"}},
		{"team alpha", "pro", store.MonthlyPlusQuotaLimitGroup, "alpha", []string{"alpha.example"}},
		{"team beta", "pro", store.MonthlyPlusQuotaLimitGroup, "beta", []string{"beta.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &store.User{Groups: []string{tc.group}}
			access := networkAccess{Mode: "personal", Allowed: true}
			if tc.team != "" {
				access.Mode, access.TeamID = "dedicated", tc.team
			}
			pools := regionalPoolsForUser(reader, access, u, tc.pkg, now)
			got := []string{}
			for _, p := range pools {
				got = append(got, p.Entry)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("discovery=%v want=%v", got, tc.want)
			}
			for _, snapshot := range reader.statuses {
				scope, valid := agentResourceScope(snapshot.Agent.Groups)
				allowed := valid && networkScopeAllows(access, snapshot.Agent.Groups) && agentResourceScopeAllowsUser(scope, access, u, tc.pkg)
				want := false
				for _, entry := range tc.want {
					want = want || entry == snapshot.Report.Xray.EntryPoint
				}
				if allowed != want {
					t.Fatalf("configuration scope %+v allowed=%v want=%v", scope, allowed, want)
				}
			}
		})
	}
	for _, class := range []string{"team", "", "unknown"} {
		if publicAgentResourceClassAllows(class, "pro") || publicAgentResourceClassAllows("shared", class) {
			t.Fatalf("unexpected public inheritance for %q", class)
		}
	}
}

func TestProNodeAPIUsesPlusAndFreeWithoutPhysicalProPool(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	now := time.Now().UTC()
	u := &store.User{Name: "pro-union", Email: "pro-union@example.test", Active: true, EmailVerified: true, ProxyUUID: "fixture-proxy", Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "pro-union-session", u.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, PackageName: "pro", IncludedQuotaBytes: 50 << 30}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 50 << 30, PeriodStart: &start, PeriodEnd: &end, SuspendState: "active"}); err != nil {
		t.Fatal(err)
	}
	reader := stubAgentStatusReader{}
	for _, class := range []string{"shared", "free", "plus"} {
		reader.statuses = append(reader.statuses, agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: class, Groups: []string{"resource:" + class}}, UpdatedAt: now,
			Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: class + ".example"}},
		})
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentStatusReader(reader))
	request := httptest.NewRequest("GET", "/api/agent-server/v1/nodes", nil)
	request.Header.Set("Authorization", "Bearer pro-union-session")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("node status=%d body=%s", response.Code, response.Body.String())
	}
	var nodes []VlessNode
	if err := json.Unmarshal(response.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, node := range nodes {
		got = append(got, node.Address)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"free.example", "plus.example", "shared.example"}) {
		t.Fatalf("nodes=%v", got)
	}
}

func TestOnlyExplicitRootRoleCanInspectCrossTeamInventory(t *testing.T) {
	for _, role := range []string{store.RoleUser, store.RoleAdmin, store.RoleOperator, store.RoleRoot} {
		for _, level := range []int{store.LevelAdmin, store.LevelOperator, store.LevelUser} {
			got := canViewRegionalPoolInventory(&store.User{Role: role, Level: level})
			if got != (role == store.RoleRoot) {
				t.Fatalf("role=%s level=%d cross-team=%v", role, level, got)
			}
		}
	}
	if canViewRegionalPoolInventory(nil) {
		t.Fatal("anonymous cross-team inventory")
	}
}

func TestTeamManagementListsRespectAssignedOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	now := time.Now().UTC()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	enrollments := st.(store.TeamAgentEnrollmentStore)
	statuses := stubAgentStatusReader{}
	for _, id := range []string{"alpha", "beta", "expired"} {
		until := now.Add(time.Hour)
		if id == "expired" {
			until = now.Add(-time.Minute)
		}
		if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 10, Active: true, ValidFrom: now.Add(-time.Hour), ValidUntil: until}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
		e, err := enrollments.RegisterTeamAgent(ctx, id, "beta")
		if err != nil {
			t.Fatal(err)
		}
		e.Status, e.AssignedTeamID = "approved", id
		if err := enrollments.DecideTeamAgent(ctx, e, e.UpdatedAt, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
		rows, err := enrollments.ListTeamAgents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range rows {
			if current.AgentID == id {
				e = current
			}
		}
		syncedAt := e.UpdatedAt.Add(time.Millisecond)
		statuses.statuses = append(statuses.statuses, agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: id, Groups: []string{"team:" + id}}, UpdatedAt: now,
			Report: agentproto.StatusReport{Healthy: true, SyncRevision: enrollmentRevision(e) + "-fixture", Xray: agentproto.XrayStatus{Running: true, LastSync: &syncedAt}},
		})
	}
	// A suggestion must never be treated as approved ownership.
	if _, err := enrollments.RegisterTeamAgent(ctx, "pending", "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, role, team string }{
		{"root", store.RoleRoot, ""}, {"admin", store.RoleAdmin, ""},
		{"admin-alpha", store.RoleAdmin, "alpha"}, {"operator-beta", store.RoleOperator, "beta"},
		{"admin-expired", store.RoleAdmin, "expired"}, {"member-alpha", store.RoleUser, "alpha"},
	} {
		u := &store.User{Name: tc.name, Email: tc.name + "@example.test", Role: tc.role, Active: true, EmailVerified: true}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, tc.name, u.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if tc.team != "" {
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: tc.team, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentStatusReader(statuses))
	for _, endpoint := range []string{"team-enrollments", "status"} {
		path := "/api/auth/admin/agents/team-enrollments"
		if endpoint == "status" {
			path = "/api/admin/agents/status"
		}
		for _, tc := range []struct {
			actor string
			want  []string
		}{
			{"root", []string{"alpha", "beta", "expired", "pending"}},
			{"admin", []string{}}, {"admin-alpha", []string{"alpha"}},
			{"operator-beta", []string{"beta"}}, {"admin-expired", []string{}},
		} {
			t.Run(endpoint+"/"+tc.actor, func(t *testing.T) {
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer "+tc.actor)
				rr := httptest.NewRecorder()
				router.ServeHTTP(rr, req)
				if rr.Code != 200 {
					t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
				}
				var out struct {
					Agents []struct{ ID, AgentID, SuggestedTeamID string }
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for _, agent := range out.Agents {
					if tc.actor != "root" && agent.SuggestedTeamID != "" {
						t.Fatal("untrusted team suggestion leaked into scoped response")
					}
					if endpoint == "status" {
						got = append(got, agent.ID)
					} else {
						got = append(got, agent.AgentID)
					}
				}
				sort.Strings(got)
				want := tc.want
				if endpoint == "status" && tc.actor == "root" {
					want = []string{"alpha", "beta", "expired"}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("saw=%v want=%v", got, want)
				}
			})
		}
		// Team membership alone does not grant access to administrative endpoints.
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer member-alpha")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != 403 {
			t.Fatalf("member admin endpoint status=%d", rr.Code)
		}
	}
}
