package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"

	"github.com/gin-gonic/gin"
)

func TestAdminAgentInventoryPermissionsAndResourceClasses(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	st := store.NewMemoryStore()
	reader := stubAgentStatusReader{}
	expected := map[string]string{
		"free": "healthy", "shared": "offline", "plus": "unhealthy", "pro": "healthy", "custom": "unknown",
	}
	for _, class := range []string{"free", "shared", "plus", "pro", "custom"} {
		snapshot := agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: class, Groups: []string{"resource:" + class}},
			Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true,
				Xray: agentproto.XrayStatus{Running: true, LastSync: &now}},
			UpdatedAt: now,
		}
		switch class {
		case "shared":
			snapshot.UpdatedAt = now.Add(-2 * regionalHeartbeatTTL)
		case "plus":
			snapshot.Report.Xray.Running = false
		case "custom":
			snapshot.UpdatedAt = time.Time{}
			snapshot.Agent.NodeID = "bound-custom"
			snapshot.Agent.AllowedEmails = []string{"private@example.test"}
		}
		reader.statuses = append(reader.statuses, snapshot)
	}
	for _, id := range []string{"*", "gateway", "team", "pending", "invalid"} {
		reader.statuses = append(reader.statuses, agentserver.StatusSnapshot{
			Agent: agentserver.Identity{ID: id, Groups: []string{"resource:shared"}},
		})
	}
	reader.statuses[6].Report.Role = "gateway"
	reader.statuses[7].Agent.Groups = []string{"team:isolated"}
	reader.statuses[8].Agent.Groups = []string{managedTeamMarker}
	reader.statuses[9].Agent.Groups = []string{"resource:free", "resource:pro"}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentStatusReader(reader))
	for _, role := range []string{store.RoleRoot, store.RoleAdmin, store.RoleOperator, store.RoleUser, store.RoleReadOnly, "anonymous"} {
		t.Run(role, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/auth/admin/agents/inventory", nil)
			if role != "anonymous" {
				user := &store.User{
					Name: role, Email: role + "@example.test", Role: role, Active: true,
					Permissions: []string{permissionAdminAgentsStatus},
				}
				if err := st.CreateUser(ctx, user); err != nil {
					t.Fatal(err)
				}
				if err := st.CreateSession(ctx, role, user.ID, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+role)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			switch role {
			case "anonymous":
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d", rec.Code)
				}
				return
			case store.RoleUser, store.RoleReadOnly:
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "private@example.test") {
				t.Fatal("inventory exposed a credential's email allowlist")
			}
			var body struct{ Agents []agentInventoryEntry }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			got := make(map[string]string)
			for _, agent := range body.Agents {
				if agent.ID != agent.ResourceClass {
					t.Errorf("wrong class for %q: %q", agent.ID, agent.ResourceClass)
				}
				if agent.ID == "custom" && (agent.NodeID != "bound-custom" || agent.LastHeartbeat != nil) {
					t.Errorf("unexpected bound node inventory: %#v", agent)
				}
				got[agent.ID] = agent.Status
			}
			if !reflect.DeepEqual(got, expected) || len(body.Agents) != len(expected) {
				t.Fatalf("inventory = %v, want %v", got, expected)
			}
		})
	}
}
