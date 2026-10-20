package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestTeamAgentEnrollmentLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	for _, id := range []string{"alpha", "beta"} {
		if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 5, Active: true, ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
		u := &store.User{Name: id, Email: id + "@example.invalid", ProxyUUID: "test-" + id, Role: store.RoleUser, Active: true, EmailVerified: true}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		for _, team := range []string{"", id} {
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: team, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	root := &store.User{Name: "root", Email: "root@example.invalid", Role: store.RoleRoot, Active: true, EmailVerified: true}
	if err := st.CreateUser(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "root-session", root.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "*", Token: "deploy-test", Groups: []string{"internal"}}, {ID: "pinned", Token: "pinned-test", Groups: []string{"team:alpha"}}}})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithAgentStatusReader(registry))
	call := func(method, path, token, class, team, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-ID", "team.example.invalid")
		req.Header.Set("X-Agent-Resource-Class", class)
		req.Header.Set("X-Agent-Team-ID", team)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	users := func(suggestion string) agentproto.ClientListResponse {
		rr := call("GET", "/api/agent-server/v1/users", "deploy-test", "team", suggestion, "")
		if rr.Code != 200 {
			t.Fatalf("users %d %s", rr.Code, rr.Body.String())
		}
		var out agentproto.ClientListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	pending := users("")
	if pending.EnrollmentStatus != "pending" || len(pending.Clients) != 0 {
		t.Fatalf("pending=%+v", pending)
	}
	if rr := call("POST", "/api/agent-server/v1/status", "deploy-test", "team", "", `{"agentId":"team.example.invalid","healthy":true,"xray":{"region":"jp","entryPoint":"team.example.invalid","running":true}}`); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	rr := call("GET", "/api/auth/admin/agents/team-enrollments", "root-session", "", "", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "team.example.invalid") {
		t.Fatalf("inventory %d %s", rr.Code, rr.Body.String())
	}
	get := func() store.TeamAgentEnrollment {
		rows, err := st.(store.TeamAgentEnrollmentStore).ListTeamAgents(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows=%v err=%v", rows, err)
		}
		return rows[0]
	}
	decide := func(status, team string, expected int) {
		e := get()
		raw, _ := json.Marshal(map[string]any{"agentId": e.AgentID, "teamId": team, "status": status, "reason": "test allocation", "updatedAt": e.UpdatedAt})
		rr := call("PUT", "/api/auth/admin/agents/team-enrollments", "root-session", "", "", string(raw))
		if rr.Code != expected {
			t.Fatalf("decision %s/%s %d: %s", status, team, rr.Code, rr.Body.String())
		}
	}
	decide("approved", "beta", 200)
	approved := users("alpha")
	if approved.EnrollmentStatus != "approved" || approved.Scope.TeamID != "beta" || len(approved.Clients) != 1 || approved.Clients[0].Email != "beta@example.invalid" {
		t.Fatalf("approved=%+v", approved)
	}
	events := call("GET", "/api/agent-server/v1/users/events", "deploy-test", "team", "alpha", "")
	if events.Code != 200 || events.Header().Get("X-Agent-Event-Mode") != "poll" || !strings.Contains(events.Body.String(), approved.Revision) {
		t.Fatal("approved event does not track user revision", events.Body.String())
	}
	// Approval must remain hidden until the node actually applies this revision.
	if got := (&handler{store: st, agentStatusReader: registry}).networkStatusReader(networkAccess{Mode: "dedicated", TeamID: "beta", Allowed: true}).Statuses(); len(got) != 0 {
		t.Fatal("unapplied approval was discoverable")
	}
	appliedAt := time.Now().UTC()
	applied, _ := json.Marshal(agentproto.StatusReport{AgentID: "team.example.invalid", Healthy: true, Users: 1, SyncRevision: approved.Revision, Xray: agentproto.XrayStatus{Running: true, Clients: 1, LastSync: &appliedAt, Region: "jp", EntryPoint: "team.example.invalid"}})
	if rr := call("POST", "/api/agent-server/v1/status", "deploy-test", "team", "alpha", string(applied)); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	if got := (&handler{store: st, agentStatusReader: registry}).networkStatusReader(networkAccess{Mode: "dedicated", TeamID: "beta", Allowed: true}).Statuses(); len(got) != 1 {
		t.Fatal("applied administrator assignment not discoverable")
	}
	// Node suggestions cannot override an administrator assignment.
	if get().AssignedTeamID != "beta" {
		t.Fatal("suggestion changed assignment")
	}
	decide("approved", "alpha", 409)
	decide("revoked", "", 200)
	revoked := users("beta")
	if revoked.EnrollmentStatus != "revoked" || len(revoked.Clients) != 0 || revoked.Revision == pending.Revision {
		t.Fatalf("revoked=%+v", revoked)
	}
	for _, snapshot := range (&handler{store: st, agentStatusReader: registry}).networkStatusReader(networkAccess{Mode: "dedicated", TeamID: "beta", Allowed: true}).Statuses() {
		if snapshot.Agent.ID == "team.example.invalid" {
			t.Fatal("revoked node remains discoverable")
		}
	}
	if rr := call("GET", "/api/agent-server/v1/users", "deploy-test", "shared", "", ""); rr.Code != 403 {
		t.Fatal("managed team downgraded to public")
	}
	decide("approved", "alpha", 409)
	// Approval cannot occur until the node acknowledges the exact empty revision.
	syncedAt := time.Now().UTC()
	payload, _ := json.Marshal(agentproto.StatusReport{AgentID: "team.example.invalid", SyncRevision: revoked.Revision, Healthy: true, Xray: agentproto.XrayStatus{Running: true, LastSync: &syncedAt}})
	if rr := call("POST", "/api/agent-server/v1/status", "deploy-test", "team", "", string(payload)); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	decide("approved", "alpha", 200)
	if out := users(""); out.Scope.TeamID != "alpha" || len(out.Clients) != 1 {
		t.Fatalf("reassigned=%+v", out)
	}
	logs, err := st.ListAuditLogs(ctx, store.AuditLogFilter{})
	if err != nil || len(logs) < 3 {
		t.Fatal("missing audit records", err)
	}
}
