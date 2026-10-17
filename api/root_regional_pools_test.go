package api

import (
	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRegionalPoolsRootInventoryDoesNotExpandUserScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	now := time.Now()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	for _, id := range []string{"alpha", "beta"} {
		if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 10, Active: true, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	users := map[string]*store.User{}
	for _, name := range []string{"root", "admin", "free", "plus", "alpha", "beta", "removed", "blocked"} {
		role := store.RoleUser
		if name == "root" {
			role = store.RoleRoot
		}
		if name == "admin" {
			role = store.RoleAdmin
		}
		u := &store.User{Name: name, Email: name + "@example.test", Role: role, Active: true, EmailVerified: true}
		if name == "plus" {
			u.Groups = []string{store.MonthlyPlusQuotaLimitGroup}
		}
		if name == "alpha" {
			u.Groups = []string{"team-admin:alpha"}
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, name, u.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		users[name] = u
		if name == "alpha" || name == "beta" || name == "removed" {
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
			if name != "removed" {
				if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: name, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := st.AddToBlacklist(ctx, users["blocked"].Email); err != nil {
		t.Fatal(err)
	}
	statuses := []agentserver.StatusSnapshot{}
	for _, id := range []string{"public", "alpha", "beta", "closed"} {
		groups := []string{"internal"}
		if id == "alpha" || id == "beta" {
			groups = []string{"access:dedicated-only", "team:" + id}
		}
		statuses = append(statuses, agentserver.StatusSnapshot{Agent: agentserver.Identity{ID: id, Groups: groups}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: id != "closed", Xray: agentproto.XrayStatus{Region: "tw", EntryPoint: id + ".example.test", Pool: id, Running: true}}})
	}
	for _, class := range []string{"free", "plus"} {
		statuses = append(statuses, agentserver.StatusSnapshot{Agent: agentserver.Identity{ID: class, Groups: []string{"resource:" + class}}, UpdatedAt: now, Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Region: "tw", EntryPoint: class + ".example.test", Running: true}}})
	}
	r := gin.New()
	RegisterRoutes(r, WithStore(st), WithAgentStatusReader(stubAgentStatusReader{statuses: statuses}))
	want := map[string][]string{
		"root":  {"alpha.example.test", "beta.example.test", "closed.example.test", "free.example.test", "plus.example.test", "public.example.test"},
		"admin": {"closed.example.test", "free.example.test", "public.example.test"},
		"free":  {"free.example.test", "public.example.test"},
		"plus":  {"plus.example.test", "public.example.test"},
		"alpha": {"alpha.example.test"}, "beta": {"beta.example.test"}, "removed": {}, "blocked": {},
	}

	for name, entries := range want {
		req := httptest.NewRequest("GET", "/api/agent-server/v1/regional-pools", nil)
		req.Header.Set("Authorization", "Bearer "+name)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		var pools []regionalPool
		if err := json.Unmarshal(rr.Body.Bytes(), &pools); err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, p := range pools {
			got = append(got, p.Entry)
		}
		if !reflect.DeepEqual(got, entries) {
			t.Fatalf("%s saw %v, want %v", name, got, entries)
		}
	}
}
