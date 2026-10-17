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
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestManagedAgentPublicClassReconcilesStaleRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	for _, id := range []string{"acme", "other"} {
		if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 10, Active: true, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"free", "plus", "free-blocked", "plus-blocked", "member", "admin", "team-blocked", "other", "registered", "sandbox-blocked"} {
		group := store.MonthlyFreeQuotaLimitGroup
		if strings.HasPrefix(name, "plus") {
			group = store.MonthlyPlusQuotaLimitGroup
		}
		groups := []string{group}
		if name == "registered" {
			groups = nil
		}
		u := &store.User{Name: name, Email: name + "@example.invalid", Active: true, EmailVerified: true, ProxyUUID: "proxy-" + name, Role: store.RoleUser, Groups: groups}
		if name == "sandbox-blocked" {
			u.Email = sandboxUserEmail
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		quota := &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 1 << 30, PeriodStart: &start, PeriodEnd: &end, SuspendState: "active"}
		if strings.HasSuffix(name, "blocked") {
			quota.ProxyAccessState = "paused"
		}
		if err := st.UpsertAccountQuotaState(ctx, quota); err != nil {
			t.Fatal(err)
		}
		if name == "member" || name == "admin" || name == "team-blocked" || name == "other" {
			team := "acme"
			if name == "other" {
				team = "other"
			}
			role := "member"
			if name == "admin" {
				role = "admin"
			}
			for _, id := range []string{"", team} {
				if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: id, DedicatedOnly: true, Role: role}, &store.AuditLog{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: "*", Token: "deployment-secret", Groups: []string{"internal"}},
		{ID: "restricted", Token: "restricted-secret", Groups: []string{"resource:free"}},
		{ID: "team-agent", Token: "team-secret", Groups: []string{"team:acme"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// This is the production failure: a public alias persisted as Free even
	// though the installer now explicitly declares shared or plus.
	registry.RegisterAgent("node-with-arbitrary-pool", []string{"resource:free"})
	registry.SetStore(st)
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry))
	server := httptest.NewServer(router)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	request := func(token, id, class, team, path string) *http.Request {
		method, body := http.MethodGet, ""
		if path == "/status" {
			method = http.MethodPost
			body = `{"agentId":"` + id + `","healthy":true}`
		}
		req, _ := http.NewRequest(method, server.URL+"/api/agent-server/v1"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Agent-ID", id)
		req.Header.Set("X-Agent-Resource-Class", class)
		req.Header.Set("X-Agent-Team-ID", team)
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	for _, tc := range []struct {
		class string
		names []string
	}{
		{"shared", []string{"free@example.invalid", "plus@example.invalid", "registered@example.invalid"}},
		{"free", []string{"free@example.invalid", "plus@example.invalid", "registered@example.invalid"}},
		{"plus", []string{"plus@example.invalid"}},
		{"shared", []string{"free@example.invalid", "plus@example.invalid", "registered@example.invalid"}},
	} {
		t.Run(tc.class, func(t *testing.T) {
			resp, err := client.Do(request("deployment-secret", "node-with-arbitrary-pool", tc.class, "", "/users"))
			if err != nil {
				t.Fatal(err)
			}
			var payload agentproto.ClientListResponse
			err = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || payload.Scope == nil || payload.Scope.ResourceClass != tc.class {
				t.Fatalf("scope response: %d %+v %v", resp.StatusCode, payload, err)
			}
			names := []string{}
			for _, c := range payload.Clients {
				names = append(names, c.Email)
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, tc.names) {
				t.Fatalf("clients=%v want=%v", names, tc.names)
			}
			resp, err = client.Do(request("deployment-secret", "node-with-arbitrary-pool", tc.class, "", "/users/events"))
			if err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(resp.Body)
			matched := false
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "data: ") {
					matched = strings.TrimPrefix(scanner.Text(), "data: ") == payload.Revision
					break
				}
			}
			resp.Body.Close()
			if !matched {
				t.Fatal("SSE did not use the same public scope")
			}
			resp, err = client.Do(request("deployment-secret", "node-with-arbitrary-pool", tc.class, "", "/status"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 204 {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			deadline := time.Now().Add(time.Second)
			for {
				persisted, err := st.GetAgent(ctx, "node-with-arbitrary-pool")
				if err == nil {
					scope, valid := agentResourceScope(persisted.Groups)
					if valid && scope.ResourceClass == tc.class {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("status did not persist the configured public class")
				}
				time.Sleep(time.Millisecond)
			}
			for _, identity := range registry.Agents() {
				if identity.ID == "node-with-arbitrary-pool" {
					scope, ok := agentResourceScope(identity.Groups)
					if !ok || scope.ResourceClass != tc.class {
						t.Fatalf("inventory scope=%+v", scope)
					}
				}
			}
		})
	}
	resp, err := client.Do(request("team-secret", "team-agent", "team", "acme", "/users"))
	if err != nil {
		t.Fatal(err)
	}
	var teamPayload agentproto.ClientListResponse
	err = json.NewDecoder(resp.Body).Decode(&teamPayload)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, c := range teamPayload.Clients {
		names = append(names, c.Email)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"admin@example.invalid", "member@example.invalid"}) {
		t.Fatalf("team clients=%v", names)
	}
	for _, tc := range []struct{ token, id, class, team string }{
		{"deployment-secret", "node-with-arbitrary-pool", "team", "acme"},
		{"deployment-secret", "team-agent", "shared", ""},
		{"restricted-secret", "restricted", "shared", ""},
		{"restricted-secret", "restricted", "plus", ""},
		{"restricted-secret", "node-with-arbitrary-pool", "shared", ""},
		{"team-secret", "team-agent", "shared", ""},
		{"team-secret", "team-agent", "team", "other"},
	} {
		for _, path := range []string{"/users", "/users/events", "/status"} {
			resp, err := client.Do(request(tc.token, tc.id, tc.class, tc.team, path))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Fatalf("unauthorized scope %s/%s %s: HTTP %d", tc.id, tc.class, path, resp.StatusCode)
			}
		}
	}
}

func TestAgentRevisionIncludesScopeForEmptyPools(t *testing.T) {
	h := &handler{store: store.NewMemoryStore()}
	_, free, err := h.authorizedAgentClients(context.Background(), []string{"resource:free"})
	if err != nil {
		t.Fatal(err)
	}
	_, shared, err := h.authorizedAgentClients(context.Background(), []string{"resource:shared"})
	if err != nil {
		t.Fatal(err)
	}
	if free == shared {
		t.Fatal("distinct scopes require distinct revisions, including empty pools")
	}
}
