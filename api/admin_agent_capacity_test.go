package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestAgentCapacityHeartbeatAndAdminInventory(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	ctx := t.Context()
	st := store.NewMemoryStore()
	now := time.Now().UTC()
	for _, role := range []string{store.RoleRoot, store.RoleOperator} {
		user := &store.User{Name: role, Email: role + "@example.invalid", Active: true, Role: role}
		if err := st.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, role, user.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: "edge.test", Token: "edge-test-token", Groups: []string{"resource:plus"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	RegisterRoutes(r, WithStore(st), WithAgentRegistry(registry), WithAgentStatusReader(registry))
	call := func(method, path, token, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("%s %s = %d (want %d): %s", method, path, rr.Code, want, rr.Body.String())
		}
		return rr
	}
	base := now.Add(-time.Second)
	if base.Month() != now.Month() {
		base = now
	}
	report := func(at time.Time, rx, tx int) {
		body := fmt.Sprintf(`{"agentId":"edge.test","role":"agent-proxy","healthy":true,"xray":{"running":true,"provider":"AWS","product":"Lightsail"},"nodeTraffic":{"interface":"eth0","bootId":"0d0d0d0d-1234-4321-9876-112233445566","rxBytes":%d,"txBytes":%d,"sampledAt":%q}}`, rx, tx, at.Format(time.RFC3339Nano))
		call(http.MethodPost, "/api/agent-server/v1/status", "edge-test-token", body, http.StatusNoContent)
	}
	report(base, 100, 200)
	report(base.Add(time.Second), 130, 220)
	report(base.Add(time.Second), 130, 220)
	limits := `{"monthlyLimitBytes":1000,"bandwidthLimitMbps":200}`
	call(http.MethodPut, "/api/auth/admin/agents/edge.test/capacity", store.RoleOperator, limits, http.StatusForbidden)
	call(http.MethodPut, "/api/auth/admin/agents/missing/capacity", store.RoleRoot, limits, http.StatusNotFound)
	call(http.MethodPut, "/api/auth/admin/agents/edge.test/capacity", store.RoleRoot, limits, http.StatusNoContent)
	rr := call(http.MethodGet, "/api/auth/admin/agents/inventory", store.RoleOperator, "", http.StatusOK)
	var response struct{ Agents []agentInventoryEntry }
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Agents) != 1 || response.Agents[0].Product != "Lightsail" || response.Agents[0].Capacity == nil {
		t.Fatalf("inventory = %s", rr.Body.String())
	}
	c := response.Agents[0].Capacity
	if c.UsedBytes == nil || *c.UsedBytes != 50 || c.RemainingBytes == nil || *c.RemainingBytes != 950 ||
		c.BandwidthLimitMbps == nil || *c.BandwidthLimitMbps != 200 {
		t.Fatalf("capacity = %s", rr.Body.String())
	}
	call(http.MethodPut, "/api/auth/admin/agents/edge.test/capacity", store.RoleRoot, `{"monthlyLimitBytes":null,"bandwidthLimitMbps":null}`, http.StatusNoContent)
	cleared, err := st.(store.AgentNodeCapacityStore).GetAgentNodeCapacity(ctx, "edge.test", now)
	if err != nil || cleared == nil || cleared.MonthlyLimitBytes != nil || cleared.BandwidthLimitMbps != nil || cleared.UsedBytes == nil || *cleared.UsedBytes != 50 {
		t.Fatalf("clearing limits changed usage: %#v, %v", cleared, err)
	}
}
