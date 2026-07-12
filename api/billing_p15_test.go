package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentserver"
	"account/internal/store"
)

// Billing P1.5 tests: suspended accounts (prolonged arrears, flag owned by
// billing-service's SuspendSyncer) must drop out of agent xray sync and
// identity enrichment, and the manual clear-arrears path must lift the
// suspension so the next sync restores access.

func seedActiveUser(t *testing.T, st store.Store, name, email, proxyUUID string) *store.User {
	t.Helper()
	user := &store.User{
		Name: name, Email: email, PasswordHash: "hashed", EmailVerified: true,
		Role: store.RoleUser, Level: store.LevelUser, Active: true, ProxyUUID: proxyUUID,
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return user
}

func suspendAccount(t *testing.T, st store.Store, accountUUID string) {
	t.Helper()
	since := time.Now().UTC().Add(-15 * 24 * time.Hour)
	if err := st.UpsertAccountQuotaState(context.Background(), &store.AccountQuotaState{
		AccountUUID:   accountUUID,
		Arrears:       true,
		ArrearsSince:  &since,
		ThrottleState: "throttled",
		SuspendState:  "suspended",
		EffectiveAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("suspend account %s: %v", accountUUID, err)
	}
}

func TestAgentUsersExcludeSuspendedAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()

	kept := seedActiveUser(t, st, "Paying User", "paying@example.com", "proxy-paying")
	cut := seedActiveUser(t, st, "Deadbeat User", "deadbeat@example.com", "proxy-deadbeat")
	suspendAccount(t, st, cut.ID)

	registry, err := agentserver.NewRegistry(agentserver.Config{
		Credentials: []agentserver.Credential{{ID: "*", Name: "test-agent", Token: "agent-token"}},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/users", nil)
	req.Header.Set("Authorization", "Bearer agent-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent users status: %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Clients []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	foundKept := false
	for _, client := range payload.Clients {
		if client.Email == "deadbeat@example.com" {
			t.Fatalf("suspended account leaked into agent sync: %#v", payload.Clients)
		}
		if client.Email == "paying@example.com" {
			foundKept = true
		}
	}
	if !foundKept {
		t.Fatalf("expected paying user %q in agent sync, got %#v", kept.Email, payload.Clients)
	}
}

func TestNetworkIdentitiesExcludeSuspendedAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "test-internal-token")
	st := store.NewMemoryStore()

	seedActiveUser(t, st, "Paying User", "paying@example.com", "proxy-paying")
	cut := seedActiveUser(t, st, "Deadbeat User", "deadbeat@example.com", "proxy-deadbeat")
	suspendAccount(t, st, cut.ID)

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/internal/network/identities", nil)
	req.Header.Set("X-Service-Token", os.Getenv("INTERNAL_SERVICE_TOKEN"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("identities status: %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Identities []struct {
			Email string `json:"email"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode identities: %v", err)
	}

	foundKept := false
	for _, identity := range payload.Identities {
		if identity.Email == "deadbeat@example.com" {
			t.Fatalf("suspended account leaked into identities: %#v", payload.Identities)
		}
		if identity.Email == "paying@example.com" {
			foundKept = true
		}
	}
	if !foundKept {
		t.Fatalf("expected paying user in identities, got %#v", payload.Identities)
	}
}

func TestAdminClearArrearsRestoresAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()

	admin := &store.User{
		Name: "Admin", Email: "admin@example.com", EmailVerified: true,
		Role: store.RoleAdmin, Level: store.LevelAdmin, Active: true,
		Permissions: []string{permissionAdminSettingsRead, permissionAdminSettingsWrite},
	}
	if err := st.CreateUser(ctx, admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	adminToken := "admin-session"
	if err := st.CreateSession(ctx, adminToken, admin.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	debtor := seedActiveUser(t, st, "Debtor", "debtor@example.com", "proxy-debtor")
	suspendAccount(t, st, debtor.ID)

	router := gin.New()
	RegisterRoutes(router, WithStore(st))

	req := httptest.NewRequest(http.MethodPost, "/api/auth/admin/billing/accounts/"+debtor.ID+"/clear-arrears", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear arrears failed: %d %s", rec.Code, rec.Body.String())
	}

	quota, err := st.GetAccountQuotaState(ctx, debtor.ID)
	if err != nil {
		t.Fatalf("load quota: %v", err)
	}
	if quota.Arrears || quota.ArrearsSince != nil || quota.ThrottleState != "normal" || quota.SuspendState != "active" {
		t.Fatalf("expected cleared dunning state, got %+v", quota)
	}

	// Unknown account -> 404.
	missReq := httptest.NewRequest(http.MethodPost, "/api/auth/admin/billing/accounts/no-such/clear-arrears", nil)
	missReq.Header.Set("Authorization", "Bearer "+adminToken)
	missRec := httptest.NewRecorder()
	router.ServeHTTP(missRec, missReq)
	if missRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown account, got %d", missRec.Code)
	}

	// Anonymous -> 401.
	anonReq := httptest.NewRequest(http.MethodPost, "/api/auth/admin/billing/accounts/"+debtor.ID+"/clear-arrears", nil)
	anonRec := httptest.NewRecorder()
	router.ServeHTTP(anonRec, anonReq)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous clear, got %d", anonRec.Code)
	}
}

func TestMarkArrearsPreservesEpisodeStart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{store: st}

	if err := h.markAccountArrears(ctx, "user-1"); err != nil {
		t.Fatalf("mark arrears: %v", err)
	}
	first, err := st.GetAccountQuotaState(ctx, "user-1")
	if err != nil || first.ArrearsSince == nil {
		t.Fatalf("expected arrears_since set, err=%v state=%+v", err, first)
	}

	// A second failure in the same episode must not push the clock forward.
	time.Sleep(5 * time.Millisecond)
	if err := h.markAccountArrears(ctx, "user-1"); err != nil {
		t.Fatalf("mark arrears again: %v", err)
	}
	second, err := st.GetAccountQuotaState(ctx, "user-1")
	if err != nil || second.ArrearsSince == nil {
		t.Fatalf("expected arrears_since preserved, err=%v state=%+v", err, second)
	}
	if !second.ArrearsSince.Equal(*first.ArrearsSince) {
		t.Fatalf("arrears_since moved: first=%v second=%v", first.ArrearsSince, second.ArrearsSince)
	}

	// Recovery clears the episode.
	plan := &store.BillingPlan{PlanID: "PRO-M", IncludedQuotaBytes: 42, PackageName: "pro"}
	if err := h.resetQuotaForPlan(ctx, "user-1", plan); err != nil {
		t.Fatalf("reset quota: %v", err)
	}
	cleared, err := st.GetAccountQuotaState(ctx, "user-1")
	if err != nil {
		t.Fatalf("load quota: %v", err)
	}
	if cleared.Arrears || cleared.ArrearsSince != nil {
		t.Fatalf("expected episode cleared, got %+v", cleared)
	}
}
