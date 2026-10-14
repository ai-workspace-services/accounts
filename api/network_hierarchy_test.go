package api

import (
	"account/internal/store"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRootTeamHierarchy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	ts := st.(store.NetworkTeamStore)
	now := time.Now()
	for _, id := range []string{"alpha", "beta"} {
		if err := ts.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 10, Active: true, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	users := map[string]*store.User{}
	for _, role := range []string{store.RoleRoot, store.RoleAdmin, store.RoleUser} {
		u := &store.User{Name: role, Email: role + "@example.invalid", Role: role, Active: true, EmailVerified: true, Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, role, u.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		users[role] = u
	}
	r := gin.New()
	RegisterRoutes(r, WithStore(st))
	call := func(role, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+role)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}
	path := "/api/auth/admin/users/" + users[store.RoleUser].ID + "/network-access"
	assign := `{"mode":"dedicated","teamId":"alpha","role":"admin","reason":"root hierarchy test"}`
	for _, role := range []string{store.RoleAdmin, store.RoleUser} {
		if rr := call(role, "PUT", path, assign); rr.Code != 403 {
			t.Fatalf("%s elevated a team administrator: %d", role, rr.Code)
		}
	}
	if rr := call(store.RoleRoot, "PUT", path, assign); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if rr := call(store.RoleUser, "GET", "/api/auth/network-team", ""); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if rr := call(store.RoleRoot, "GET", "/api/auth/network-team?teamId=alpha", ""); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	// Omitted roles preserve the existing same-team administrator assignment.
	if rr := call(store.RoleRoot, "PUT", path, `{"mode":"dedicated","teamId":"alpha","reason":"preserve role"}`); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	u, _ := st.GetUserByID(ctx, users[store.RoleUser].ID)
	if !networkTeamAdmin(u, "alpha") || store.MonthlyQuotaGroup(u) != store.MonthlyPlusQuotaLimitGroup {
		t.Fatal("team assignment changed role or subscription")
	}
	// Root can move users across teams and revoke the previous team's privilege.
	if rr := call(store.RoleRoot, "PUT", path, `{"mode":"dedicated","teamId":"beta","role":"member","reason":"move team"}`); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	u, _ = st.GetUserByID(ctx, u.ID)
	if networkTeamAdmin(u, "alpha") || networkTeamAdmin(u, "beta") {
		t.Fatal("stale team privilege retained")
	}
	rr := call(store.RoleRoot, "GET", path, "")
	var access networkAccess
	if err := json.Unmarshal(rr.Body.Bytes(), &access); err != nil {
		t.Fatal(err)
	}
	if access.Role != "member" || access.TeamID != "beta" {
		t.Fatalf("incorrect hierarchy: %+v", access)
	}
	if rr := call(store.RoleUser, "GET", "/api/auth/network-team?teamId=alpha", ""); rr.Code != 403 {
		t.Fatal("member bypassed team scope")
	}
}
