package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestNetworkScopeMatrix(t *testing.T) {
	personal := networkAccess{Mode: "personal", Allowed: true}
	team := networkAccess{Mode: "dedicated", TeamID: "acme", Allowed: true}
	revoked := networkAccess{Mode: "dedicated", Allowed: false}
	for _, tc := range []struct {
		access networkAccess
		groups []string
		want   bool
	}{
		{personal, nil, true}, {personal, []string{"internal"}, true}, {personal, []string{"team:acme"}, false},
		{team, nil, false}, {team, []string{"team:acme"}, true}, {team, []string{"team:other"}, false},
		{revoked, nil, false}, {revoked, []string{"team:acme"}, false},
		{personal, []string{"access:dedicated-only"}, false}, {team, []string{"team:acme", "team:other"}, false},
		{personal, []string{"team-admin:acme"}, false}, {personal, []string{"TEAM:acme"}, false},
		{personal, []string{"Access:Dedicated-Only"}, false},
		{team, []string{"team:acme", "team-admin:other"}, false},
	} {
		if got := networkScopeAllows(tc.access, tc.groups); got != tc.want {
			t.Errorf("%+v %v got %v", tc.access, tc.groups, got)
		}
	}
}

func TestAgentResourceScopeFiltersPublicEntitlements(t *testing.T) {
	free := &store.User{Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	plus := &store.User{Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	pro := &store.User{Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	personal := networkAccess{Mode: "personal", Allowed: true}
	for _, tc := range []struct {
		name        string
		groups      []string
		user        *store.User
		packageName string
		want        bool
	}{
		{"free accepts free", []string{"resource:free"}, free, "free", true},
		{"free accepts plus", []string{"resource:free"}, plus, "plus", true},
		{"plus accepts plus", []string{"resource:plus"}, plus, "plus", true},
		{"plus rejects free", []string{"resource:plus"}, free, "free", false},
		{"plus excludes pro", []string{"resource:plus"}, pro, "pro", false},
		{"pro accepts pro", []string{"resource:pro"}, pro, "pro", true},
		{"pro rejects plus", []string{"resource:pro"}, plus, "plus", false},
		{"shared accepts personal", []string{"resource:shared"}, plus, "plus", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, ok := agentResourceScope(tc.groups)
			got := ok && agentResourceScopeAllowsUser(scope, personal, tc.user, tc.packageName)
			if got != tc.want {
				t.Fatalf("scope=%+v valid=%v allowed=%v", scope, ok, got)
			}
		})
	}
}

func TestNetworkTeamAdminAndQuotaGroups(t *testing.T) {
	admin := &store.User{Groups: []string{"team:acme", "team-admin:acme"}}
	if !networkTeamAdmin(admin, "acme") {
		t.Fatal("team-admin marker should grant team management")
	}
	if networkTeamAdmin(admin, "other") {
		t.Fatal("team-admin marker must be scoped to its team")
	}
	legacy := &store.User{Groups: []string{"team:acme:admin"}}
	if !networkTeamAdmin(legacy, "acme") {
		t.Fatal("legacy team admin marker should remain compatible")
	}

	user := &store.User{Groups: []string{"segment:keep", store.MonthlyFreeQuotaLimitGroup, "team:acme"}}
	if group, label := teamQuotaGroup(user); group != "3gb" || label != "Free" {
		t.Fatalf("free quota mapping: %q %q", group, label)
	}
	if err := setTeamQuotaGroup(user, "20gb"); err != nil {
		t.Fatal(err)
	}
	if got := store.MonthlyQuotaGroup(user); got != store.MonthlyPlusQuotaLimitGroup {
		t.Fatalf("quota group not replaced: %q", got)
	}
	if err := setTeamQuotaGroup(user, "invalid"); err == nil {
		t.Fatal("invalid quota group accepted")
	}
}

func TestNetworkAgentsAndSnapshotsRemainIsolated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	for _, id := range []string{"acme", "other"} {
		if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 10, Active: true, ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	users := map[string]*store.User{}
	for _, id := range []string{"personal", "acme", "other", "revoked"} {
		u := &store.User{Name: id, Email: id + "@example.invalid", Active: true, EmailVerified: true, Role: store.RoleUser}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		users[id] = u
		if id != "personal" {
			teamID := id
			if id == "revoked" {
				teamID = ""
			}
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: teamID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.CreateSession(ctx, "session-"+id, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: "public.example", Token: "public-token"}, {ID: "acme.example", Token: "acme-token", Groups: []string{"team:acme"}}, {ID: "other.example", Token: "other-token", Groups: []string{"team:other"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"public", "acme", "other"} {
		identity, _ := registry.Authenticate(id + "-token")
		registry.ReportStatus(*identity, agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "team-zone", EntryPoint: id + ".example"}})
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithAgentStatusReader(registry), WithEmailVerification(false))
	get := func(path, token, alias string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if alias != "" {
			req.Header.Set("X-Agent-ID", alias)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	for _, id := range []string{"public", "acme", "other"} {
		rr := get("/api/agent-server/v1/users", id+"-token", "")
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", id, rr.Code, rr.Body.String())
		}
		var payload agentproto.ClientListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		want := id
		if id == "public" {
			want = "personal"
		}
		if len(payload.Clients) != 1 || payload.Clients[0].Email != users[want].Email {
			t.Fatalf("%s leaked clients: %+v", id, payload)
		}
	}
	for _, tc := range []struct{ token, alias string }{{"public-token", "acme.example"}, {"acme-token", "other.example"}} {
		if rr := get("/api/agent-server/v1/users", tc.token, tc.alias); rr.Code != 403 {
			t.Fatalf("identity alias accepted: %d %s", rr.Code, rr.Body.String())
		}
	}
	for _, id := range []string{"acme", "other"} {
		rr := get("/api/agent-server/v1/nodes", "session-"+id, "")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), id+".example") || strings.Contains(rr.Body.String(), "public.example") {
			t.Fatalf("nodes: %d %s", rr.Code, rr.Body.String())
		}
		rr = get("/api/auth/sync/config?since_version=9999999999", "session-"+id, "")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), id+".example") || strings.Contains(rr.Body.String(), "public.example") {
			t.Fatalf("config: %d %s", rr.Code, rr.Body.String())
		}
		var snapshot struct {
			RenderedJSON string `json:"rendered_json"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(snapshot.RenderedJSON, `"tag":"proxy"`) || strings.Contains(snapshot.RenderedJSON, `"protocol":"freedom"`) {
			t.Fatalf("invalid team client config: %s", snapshot.RenderedJSON)
		}
	}
	if rr := get("/api/agent-server/v1/nodes", "session-revoked", ""); rr.Code != 403 {
		t.Fatalf("revoked: %d %s", rr.Code, rr.Body.String())
	}
	teamList, _ := teams.ListNetworkTeams(ctx)
	team := teamList[0]
	team.ValidUntil = time.Now().Add(-time.Minute)
	if err := teams.PutNetworkTeam(ctx, team, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if rr := get("/api/agent-server/v1/nodes", "session-acme", ""); rr.Code != 403 {
		t.Fatalf("expired: %d %s", rr.Code, rr.Body.String())
	}
}

func TestDedicatedNodeOutageNeverFallsBack(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	u := &store.User{Name: "team outage", Email: "outage@example.invalid", Active: true, EmailVerified: true, Role: store.RoleUser}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: "acme", Name: "ACME", MemberLimit: 1, Active: true, ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour)}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: "acme", DedicatedOnly: true}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "outage-token", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithServerPublicURL("https://public.example"))
	for _, path := range []string{"/api/agent-server/v1/nodes", "/api/auth/sync/config"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer outage-token")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != 503 || strings.Contains(rr.Body.String(), "public.example") {
			t.Fatalf("public fallback: %d %s", rr.Code, rr.Body.String())
		}
	}
}

func TestActiveTeamProtectsInfrequentFreeMembersFromArchival(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	old := time.Now().Add(-101 * 24 * time.Hour)
	user := &store.User{Name: "infrequent team member", Email: "infrequent@example.invalid", Active: true, EmailVerified: true, Groups: []string{store.MonthlyFreeQuotaLimitGroup}, LastActiveAt: &old}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: "acme", Name: "ACME", MemberLimit: 1, Active: false, ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour)}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: user.ID, TeamID: "acme"}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	list, _ := teams.ListNetworkTeams(ctx)
	team := list[0]
	team.Active = true
	if err := teams.PutNetworkTeam(ctx, team, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	if err := h.archiveInactiveFreeUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetUserByID(ctx, user.ID)
	if err != nil || !fresh.Active || fresh.ArchivedAt != nil {
		t.Fatalf("active enterprise member archived: %+v %v", fresh, err)
	}
}
