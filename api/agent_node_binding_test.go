package api

import (
	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNodeBoundCredentialsIsolateSameHost(t *testing.T) {
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: "a", NodeID: "node-a", Token: "test-a", Groups: []string{"resource:plus"}},
		{ID: "b", NodeID: "node-b", Token: "test-b", Groups: []string{"resource:custom", "custom:allow-free"}, AllowedEmails: []string{"member@example.invalid"}},
		{ID: "*", Token: "test-legacy", Groups: []string{"internal"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore()
	for _, email := range []string{"member@example.invalid", "attacker@example.invalid"} {
		if err := st.CreateUser(context.Background(), &store.User{Name: email, Email: email, Active: true, EmailVerified: true, ProxyUUID: "test-" + email}); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry))
	for _, tc := range []struct {
		name, token, id, node, class, path string
		status                             int
	}{
		{"a status", "test-a", "a", "node-a", "plus", "status", 204},
		{"b status", "test-b", "b", "node-b", "custom", "status", 204},
		{"cross node", "test-a", "a", "node-b", "plus", "status", 403},
		{"cross agent", "test-a", "b", "node-a", "plus", "status", 403},
		{"event identity", "test-a", "b", "node-a", "plus", "users/events", 403},
		{"event scope", "test-a", "a", "node-a", "free", "users/events", 403},
		{"cross scope", "test-a", "a", "node-a", "free", "users", 403},
		{"legacy team takeover", "test-legacy", "a", "node-a", "team", "users", 403},
		{"legacy takeover", "test-legacy", "a", "node-a", "plus", "status", 403},
		{"a receipt", "test-a", "a", "node-a", "plus", "users", 200},
		{"b receipt", "test-b", "b", "node-b", "custom", "users", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodGet
			body := ""
			if tc.path == "status" {
				method = http.MethodPost
				body = `{"agentId":"` + tc.id + `","healthy":true,"xray":{"nodeId":"` + tc.node + `"}}`
			}
			req := httptest.NewRequest(method, "/api/agent-server/v1/"+tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("X-Agent-ID", tc.id)
			req.Header.Set("X-Agent-Resource-Class", tc.class)
			req.Header.Set("X-Agent-Allowed-Emails", `["attacker@example.invalid"]`)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("HTTP %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.path == "users" && w.Code == 200 {
				var got agentproto.ClientListResponse
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if tc.id == "b" {
					if got.Scope == nil || !got.Scope.CustomAllowFree || len(got.Clients) != 1 || got.Clients[0].Email != "member@example.invalid" {
						t.Fatalf("enrolled custom audience lost or expanded: %+v", got)
					}
				}
				if got.NodeID != tc.node || got.AgentID != tc.id {
					t.Fatalf("wrong receipt: %+v", got)
				}
			}
		})
	}
	for _, selector := range []struct{ path, header, body string }{
		{"/status", "b", `{"agentId":"a","xray":{"nodeId":"node-a"}}`},
		{"/status?agentId=b", "a", `{"agentId":"a","xray":{"nodeId":"node-a"}}`},
		{"/users?agentId=b", "a", ""},
	} {
		method := http.MethodGet
		if selector.body != "" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, "/api/agent-server/v1"+selector.path, strings.NewReader(selector.body))
		req.Header.Set("Authorization", "Bearer test-a")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-ID", selector.header)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("conflicting selector accepted: %s HTTP %d", selector.path, w.Code)
		}
	}
	statuses := registry.Statuses()
	count := 0
	for _, s := range statuses {
		if !s.UpdatedAt.IsZero() {
			count++
			if s.Report.Xray.NodeID != s.Agent.NodeID {
				t.Fatal("cross-node status")
			}
		}
	}
	if count != 2 {
		t.Fatalf("want independent live nodes, got %d", count)
	}
}
