package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"account/internal/store"
)

const opsTestPassword = "scrubbed"

// opsHarness wires a router with an admin session plus one ordinary target
// account, which is the shape every ops endpoint is exercised against.
func opsHarness(t *testing.T) (*gin.Engine, store.Store, string, *store.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	hashed, err := bcrypt.GenerateFromPassword([]byte(opsTestPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	admin := &store.User{
		ID: "admin-1", Name: "administrator", Email: "admin@example.com",
		PasswordHash: string(hashed), EmailVerified: true, Role: store.RoleAdmin,
	}
	if err := st.CreateUser(context.Background(), admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	target := &store.User{
		ID: "user-1", Name: "target", Email: "target@example.com",
		PasswordHash: string(hashed), EmailVerified: true, Role: store.RoleUser,
	}
	if err := st.CreateUser(context.Background(), target); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"identifier": admin.Email, "password": opsTestPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}
	token := decodeResponse(t, rec).Token
	if token == "" {
		t.Fatal("expected a session token")
	}
	return router, st, token, target
}

func opsPost(t *testing.T, router *gin.Engine, token, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func seedTrialPlan(t *testing.T, st store.Store) {
	t.Helper()
	plan := &store.BillingPlan{
		PlanID: store.BillingPlanTrial7D, DisplayName: "7-Day Trial", Kind: "trial",
		IncludedQuotaBytes: 10 << 30, PackageName: "trial", TrialDays: 7, Active: true,
	}
	if err := st.UpsertBillingPlan(context.Background(), plan); err != nil {
		t.Fatalf("seed trial plan: %v", err)
	}
}

// Every ops write is answerable later, so a missing reason must be rejected
// before anything changes — not merely discouraged in the UI.
func TestOpsWritesRejectMissingReason(t *testing.T) {
	router, st, token, target := opsHarness(t)
	seedTrialPlan(t, st)

	cases := []struct {
		name    string
		path    string
		payload any
	}{
		{"assign plan", "/api/auth/admin/billing/accounts/user-1/plan",
			map[string]any{"planId": store.BillingPlanTrial7D}},
		{"adjust quota", "/api/auth/admin/billing/accounts/user-1/quota",
			map[string]any{"remainingIncludedQuota": 1024}},
		{"adjust balance", "/api/auth/admin/billing/accounts/user-1/balance",
			map[string]any{"delta": 10.0}},
		{"grant trial", "/api/auth/admin/billing/accounts/user-1/grant-trial",
			map[string]any{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := opsPost(t, router, token, tc.path, tc.payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 without a reason, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Nothing should have been written by any of the rejected calls.
	entries, err := st.ListAuditLogs(context.Background(), store.AuditLogFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no audit entries after rejected writes, got %d", len(entries))
	}
	if state, err := st.GetAccountQuotaState(context.Background(), target.ID); err == nil && state != nil {
		if state.RemainingIncludedQuota != 0 || state.CurrentBalance != 0 {
			t.Fatalf("rejected writes still mutated quota state: %+v", state)
		}
	}
}

func TestOpsAdjustQuotaRecordsAudit(t *testing.T) {
	router, st, token, _ := opsHarness(t)

	rec := opsPost(t, router, token, "/api/auth/admin/billing/accounts/user-1/quota",
		map[string]any{"remainingIncludedQuota": 21474836480, "reason": "goodwill top-up, ticket #1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	state, err := st.GetAccountQuotaState(context.Background(), "user-1")
	if err != nil || state == nil {
		t.Fatalf("expected quota state, err=%v", err)
	}
	if state.RemainingIncludedQuota != 21474836480 {
		t.Fatalf("quota not applied, got %d", state.RemainingIncludedQuota)
	}

	entries, err := st.ListAuditLogs(context.Background(),
		store.AuditLogFilter{ActionPrefix: store.AuditActionQuotaAdjust})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.ActorUUID != "admin-1" {
		t.Fatalf("expected actor admin-1, got %q", entry.ActorUUID)
	}
	if entry.Details["target_uuid"] != "user-1" {
		t.Fatalf("expected target user-1, got %v", entry.Details["target_uuid"])
	}
	if entry.Details["reason"] != "goodwill top-up, ticket #1" {
		t.Fatalf("reason not recorded, got %v", entry.Details["reason"])
	}
	if _, ok := entry.Details["before"]; !ok {
		t.Fatal("audit entry is missing the before value")
	}
	if _, ok := entry.Details["after"]; !ok {
		t.Fatal("audit entry is missing the after value")
	}
}

// Balance must stay equal to the sum of its ledger, otherwise reconciliation
// can no longer explain a difference.
func TestOpsAdjustBalanceWritesLedgerEntry(t *testing.T) {
	router, st, token, _ := opsHarness(t)

	rec := opsPost(t, router, token, "/api/auth/admin/billing/accounts/user-1/balance",
		map[string]any{"delta": 50.0, "reason": "refund for incident #42"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	state, err := st.GetAccountQuotaState(context.Background(), "user-1")
	if err != nil || state == nil {
		t.Fatalf("expected quota state, err=%v", err)
	}
	if state.CurrentBalance != 50.0 {
		t.Fatalf("expected balance 50, got %v", state.CurrentBalance)
	}

	ledger, err := st.ListBillingLedgerByAccount(context.Background(), "user-1", 10)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("expected one ledger entry, got %d", len(ledger))
	}
	if ledger[0].EntryType != "manual_adjustment" {
		t.Fatalf("unexpected entry type %q", ledger[0].EntryType)
	}
	if ledger[0].AmountDelta != 50.0 || ledger[0].BalanceAfter != 50.0 {
		t.Fatalf("ledger does not reconcile with balance: %+v", ledger[0])
	}

	// A second adjustment must accumulate rather than overwrite.
	rec = opsPost(t, router, token, "/api/auth/admin/billing/accounts/user-1/balance",
		map[string]any{"delta": -20.0, "reason": "correcting overcredit"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on second adjustment, got %d: %s", rec.Code, rec.Body.String())
	}
	state, _ = st.GetAccountQuotaState(context.Background(), "user-1")
	if state.CurrentBalance != 30.0 {
		t.Fatalf("expected balance 30 after -20, got %v", state.CurrentBalance)
	}
}

func TestOpsGrantTrialAppliesEntitlements(t *testing.T) {
	router, st, token, _ := opsHarness(t)
	seedTrialPlan(t, st)

	rec := opsPost(t, router, token, "/api/auth/admin/billing/accounts/user-1/grant-trial",
		map[string]any{"reason": "sales demo account"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	profile, err := st.GetAccountBillingProfile(context.Background(), "user-1")
	if err != nil || profile == nil {
		t.Fatalf("expected a billing profile after granting a trial, err=%v", err)
	}
	if profile.PackageName != "trial" {
		t.Fatalf("expected package trial, got %q", profile.PackageName)
	}
	state, err := st.GetAccountQuotaState(context.Background(), "user-1")
	if err != nil || state == nil {
		t.Fatalf("expected quota state, err=%v", err)
	}
	if state.RemainingIncludedQuota != 10<<30 {
		t.Fatalf("trial quota not applied, got %d", state.RemainingIncludedQuota)
	}
	if state.PeriodEnd == nil {
		t.Fatal("expected the trial grant to bound the period")
	}

	entries, _ := st.ListAuditLogs(context.Background(),
		store.AuditLogFilter{ActionPrefix: store.AuditActionTrialGrant})
	if len(entries) != 1 {
		t.Fatalf("expected one trial audit entry, got %d", len(entries))
	}
}

// The root account is protected from operator writes the same way role and
// group changes already protect it.
func TestOpsWritesRejectRootTarget(t *testing.T) {
	router, st, token, _ := opsHarness(t)
	root := &store.User{
		ID: "root-1", Name: "root", Email: "root@example.com",
		EmailVerified: true, Role: store.RoleRoot,
	}
	if err := st.CreateUser(context.Background(), root); err != nil {
		t.Fatalf("seed root: %v", err)
	}

	rec := opsPost(t, router, token, "/api/auth/admin/billing/accounts/root-1/balance",
		map[string]any{"delta": 100.0, "reason": "should not be allowed"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for root target, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpsAuditListFiltersByTarget(t *testing.T) {
	router, st, token, _ := opsHarness(t)

	other := &store.User{
		ID: "user-2", Name: "other", Email: "other@example.com",
		EmailVerified: true, Role: store.RoleUser,
	}
	if err := st.CreateUser(context.Background(), other); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	opsPost(t, router, token, "/api/auth/admin/billing/accounts/user-1/quota",
		map[string]any{"remainingIncludedQuota": 100, "reason": "first"})
	opsPost(t, router, token, "/api/auth/admin/billing/accounts/user-2/quota",
		map[string]any{"remainingIncludedQuota": 200, "reason": "second"})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/admin/audit?target=user-2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Entries []store.AuditLog `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Entries) != 1 {
		t.Fatalf("expected one entry for user-2, got %d", len(payload.Entries))
	}
	if payload.Entries[0].Details["reason"] != "second" {
		t.Fatalf("wrong entry returned: %v", payload.Entries[0].Details)
	}
}
