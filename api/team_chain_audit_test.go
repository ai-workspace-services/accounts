package api

import (
	"account/internal/store"
	"context"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuditTeamChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	seedAdminPlanGroupCatalog(t, st)
	ts := st.(store.NetworkTeamStore)
	now := time.Now()
	for _, id := range []string{"alpha", "beta"} {
		if err := ts.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, MemberLimit: 2, Active: true, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	users := map[string]*store.User{}
	for _, name := range []string{"owner", "member", "other", "overflow"} {
		u := &store.User{Name: name, Email: name + "@example.invalid", Role: store.RoleUser, Active: true, EmailVerified: true}
		if name == "owner" {
			u.Groups = []string{"team-admin:alpha"}
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		users[name] = u
	}
	for name, id := range map[string]string{"owner": "alpha", "other": "beta"} {
		if err := ts.PutNetworkMembership(ctx, store.NetworkMembership{UserID: users[name].ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
		if err := ts.PutNetworkMembership(ctx, store.NetworkMembership{UserID: users[name].ID, TeamID: id, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateSession(ctx, "audit-owner", users["owner"].ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	RegisterRoutes(r, WithStore(st))
	call := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer audit-owner")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		t.Logf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
		return rr.Code
	}
	if code := call("POST", "/api/auth/network-team/members", `{"email":"member@example.invalid"}`); code != 201 {
		t.Fatal(code)
	}
	h := &handler{store: st}
	has := func(scope, name string) bool {
		cs, _, err := h.authorizedAgentClients(ctx, []string{"team:" + scope})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Email == users[name].Email {
				return true
			}
		}
		return false
	}
	if !has("alpha", "member") || has("beta", "member") {
		t.Fatal("initial isolation failed")
	}
	path := "/api/auth/network-team/members/" + users["member"].ID
	if call("PUT", path, `{"paused":true}`) != 200 || has("alpha", "member") {
		t.Fatal("pause failed")
	}
	if call("PUT", path, `{"paused":false}`) != 200 || !has("alpha", "member") {
		t.Fatal("resume failed")
	}
	if call("PUT", "/api/auth/network-team/members/"+users["other"].ID, `{"paused":true}`) != 404 {
		t.Fatal("cross-team mutation allowed")
	}
	if call("PUT", path, `{"quotaGroup":"20gb"}`) != 200 {
		t.Fatal("quota request failed")
	}
	profile, _ := st.GetAccountBillingProfile(ctx, users["member"].ID)
	quota, _ := st.GetAccountQuotaState(ctx, users["member"].ID)
	t.Logf("after 20gb selection billing profile=%+v quota=%+v", profile, quota)
	if profile == nil || profile.IncludedQuotaBytes != 20*adminPlanTestGiB || quota.RemainingIncludedQuota != 20*adminPlanTestGiB {
		t.Fatal("quota entitlement not synchronized")
	}
	if call("POST", "/api/auth/network-team/members", `{"email":"overflow@example.invalid"}`) != 409 {
		t.Fatal("capacity not enforced")
	}
	overflow, _ := ts.GetNetworkMembership(ctx, users["overflow"].ID)
	t.Logf("failed invite left personal account membership=%+v", overflow)
	if overflow.Configured || overflow.DedicatedOnly {
		t.Fatal("failed invitation changed personal access")
	}
	if call("DELETE", path, "") != 200 || has("alpha", "member") {
		t.Fatal("remove failed")
	}
}
