package api

import (
	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"bufio"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestPublicEmailUnionBusinessPaths(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	users := map[string]*store.User{}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	for _, name := range []string{"plus", "pro", "alice", "bob", "outsider", "paused", "exhausted", "inactive", "unverified", "dedicated"} {
		group := store.MonthlyFreeQuotaLimitGroup
		if name == "plus" || name == "pro" {
			group = store.MonthlyPlusQuotaLimitGroup
		}
		u := &store.User{Name: name, Email: name + "@example.invalid", Active: name != "inactive", EmailVerified: name != "unverified", ProxyUUID: "test-" + name, Groups: []string{group}, Role: store.RoleUser}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		users[name] = u
		if name == "inactive" {
			if err := st.SetUserActiveWithAudit(ctx, u.ID, false, "test-admin"); err != nil {
				t.Fatal(err)
			}
		}
		if name != "pro" {
			if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, PackageName: "free", IncludedQuotaBytes: 1 << 30}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.CreateSession(ctx, "session-"+name, u.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		q := &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 1 << 30, PeriodStart: &start, PeriodEnd: &end, SuspendState: "active"}
		if name == "paused" {
			q.ProxyAccessState = "paused"
		}
		if name == "exhausted" {
			q.RemainingIncludedQuota = 0
		}
		if err := st.UpsertAccountQuotaState(ctx, q); err != nil {
			t.Fatal(err)
		}
		if name == "pro" {
			if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, PackageName: "pro", IncludedQuotaBytes: 1 << 30}); err != nil {
				t.Fatal(err)
			}
		}
	}
	teams := st.(store.NetworkTeamStore)
	if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: "acme", Name: "Acme", MemberLimit: 10, Active: true, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: users["dedicated"].ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: users["dedicated"].ID, TeamID: "acme", DedicatedOnly: true, Role: "member"}, &store.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "*", Token: "test-manager", Groups: []string{"internal"}}, {ID: "restricted", Token: "test-restricted", Groups: []string{"resource:plus"}}}})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithAgentStatusReader(registry), WithServerPublicURL("https://accounts.example.invalid"))
	list := `[" Alice@Example.Invalid ","bob@example.invalid","paused@example.invalid","exhausted@example.invalid","inactive@example.invalid","unverified@example.invalid","dedicated@example.invalid"]`
	agentRequest := func(token, node, path, emails string) *http.Request {
		method := http.MethodGet
		body := ""
		if path == "status" {
			method = http.MethodPost
			body = `{"agentId":"` + node + `","healthy":true,"xray":{"nodeId":"` + node + `","running":true,"region":"us","entryPoint":"` + node + `.example.invalid"}}`
		}
		req := httptest.NewRequest(method, "/api/agent-server/v1/"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Agent-ID", node)
		req.Header.Set("X-Agent-Resource-Class", "plus")
		if emails != "" {
			req.Header.Set("X-Agent-Email-Policy", agentproto.PublicEmailUnionPolicy)
			req.Header.Set("X-Agent-Allowed-Emails", emails)
		}
		return req
	}
	request := func(req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, node := range []string{"union", "ordinary"} {
		emails := ""
		if node == "union" {
			emails = list
		}
		w := request(agentRequest("test-manager", node, "status", emails))
		if w.Code != 204 {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
	}
	snapshot := func(emails string) agentproto.ClientListResponse {
		w := request(agentRequest("test-manager", "union", "users", emails))
		if w.Code != 200 {
			t.Fatalf("users %d %s", w.Code, w.Body.String())
		}
		var out agentproto.ClientListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot(list)
	got := []string{}
	for _, c := range before.Clients {
		got = append(got, c.Email)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "alice@example.invalid,bob@example.invalid,plus@example.invalid,pro@example.invalid" {
		t.Fatalf("union or mandatory gate wrong: %v", got)
	}
	if before.Scope == nil || before.Scope.EmailPolicy != agentproto.PublicEmailUnionPolicy {
		t.Fatal("missing policy receipt")
	}
	for _, tc := range []struct {
		name string
		want int
	}{{"alice", 1}, {"bob", 1}, {"outsider", 0}, {"plus", 2}, {"pro", 2}} {
		for _, path := range []string{"/api/agent-server/v1/nodes", "/api/auth/sync/config?since_version=0", "/api/agent-server/v1/regional-pools"} {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Authorization", "Bearer session-"+tc.name)
			w := request(req)
			if w.Code != 200 {
				t.Fatalf("%s %s HTTP %d %s", tc.name, path, w.Code, w.Body.String())
			}
			var payload any
			json.Unmarshal(w.Body.Bytes(), &payload)
			var entries []any
			if strings.Contains(path, "sync/config") {
				entries = payload.(map[string]any)["nodes"].([]any)
			} else {
				entries = payload.([]any)
			}
			if len(entries) != tc.want {
				t.Fatalf("%s %s nodes=%d want=%d", tc.name, path, len(entries), tc.want)
			}
		}
	}
	// SSE's initial revision must describe the same union snapshot.
	server := httptest.NewServer(router)
	defer server.Close()
	streamCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req := agentRequest("test-manager", "union", "users/events", list)
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(server.URL, "http://")
	req.RequestURI = ""
	req = req.WithContext(streamCtx)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	event := ""
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			event = strings.TrimPrefix(scanner.Text(), "data: ")
			break
		}
	}
	resp.Body.Close()
	cancel()
	if event != before.Revision {
		t.Fatal("event revision differs from union snapshot")
	}
	if w := request(agentRequest("test-restricted", "restricted", "users", list)); w.Code != 403 {
		t.Fatalf("restricted credential expanded audience: %d", w.Code)
	}
	for _, raw := range []string{`["*@example.invalid"]`, `["invalid"]`} {
		if w := request(agentRequest("test-manager", "union", "users", raw)); w.Code != 400 {
			t.Fatalf("invalid extension accepted: %d", w.Code)
		}
	}
	for _, name := range []string{"alice", "bob"} {
		u, err := st.GetUserByID(ctx, users[name].ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(u.Groups, ",") != store.MonthlyFreeQuotaLimitGroup {
			t.Fatal("exception changed user groups")
		}
		profile, err := st.GetAccountBillingProfile(ctx, u.ID)
		if err != nil || profile.PackageName != "free" {
			t.Fatal("exception upgraded user plan")
		}
	}
	after := snapshot("[]")
	if after.Revision == before.Revision || len(after.Clients) != 2 {
		t.Fatal("email removal did not revoke Free client")
	}
	if w := request(agentRequest("test-manager", "union", "status", "[]")); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	alice := users["alice"]
	pools := regionalPoolsForUser(registry, networkAccess{Mode: "personal", Allowed: true}, alice, "free", time.Now().UTC())
	if len(pools) != 0 {
		t.Fatal("removed email still sees node")
	}
}
