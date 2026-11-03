package api

import (
	"account/internal/store"
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResetMonthlyUsageRoleEmailAndFinancialState(t *testing.T) {
	for _, role := range []string{store.RoleRoot, store.RoleAdmin, store.RoleOperator, store.RoleUser} {
		t.Run(role, func(t *testing.T) {
			router, st, _, target := opsHarness(t)
			ctx := context.Background()
			actor := &store.User{Name: "actor-" + role, Email: role + "@example.invalid", Role: role, Active: true}
			if err := st.CreateUser(ctx, actor); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateSession(ctx, "reset-"+role, actor.ID, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			_ = st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: target.ID, IncludedQuotaBytes: 100})
			_ = st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: target.ID, RemainingIncludedQuota: 4, CurrentBalance: -2, Arrears: true, SuspendState: "suspended", ProxyAccessState: "paused"})
			call := func(email, reason string) *httptest.ResponseRecorder {
				b, _ := json.Marshal(map[string]string{"email": email, "reason": reason})
				req := httptest.NewRequest("POST", "/api/auth/admin/billing/accounts/"+target.ID+"/reset-monthly-usage", bytes.NewReader(b))
				req.Header.Set("Authorization", "Bearer reset-"+role)
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			if role == store.RoleUser {
				if w := call(target.Email, "test"); w.Code != 403 {
					t.Fatalf("ordinary user: %d", w.Code)
				}
				return
			}
			if w := call("wrong@example.invalid", "test"); w.Code != 409 {
				t.Fatalf("email mismatch: %d %s", w.Code, w.Body.String())
			}
			if w := call(target.Email, ""); w.Code != 400 {
				t.Fatalf("missing reason: %d", w.Code)
			}
			if w := call(target.Email, "support correction"); w.Code != 200 {
				t.Fatalf("reset failed: %d %s", w.Code, w.Body.String())
			}
			q, _ := st.GetAccountQuotaState(ctx, target.ID)
			if q.RemainingIncludedQuota != 100 || q.CurrentBalance != -2 || !q.Arrears || q.SuspendState != "suspended" || q.ProxyAccessState != "paused" {
				t.Fatalf("unexpected state: %+v", q)
			}
		})
	}
}
