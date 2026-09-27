package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// createUserSpy records the user each caller hands to CreateUser before the
// backing store gets to normalize it.
//
// The assertion has to happen here rather than by reading the account back:
// memoryStore.CreateUser force-sets Active = true, so a round-trip through it
// reports an active account no matter what the caller passed. The Postgres
// store has no such rescue — it writes the field verbatim, which is how a
// self-registered account reached production with active = false.
type createUserSpy struct {
	store.Store

	created []store.User
}

func (s *createUserSpy) CreateUser(ctx context.Context, user *store.User) error {
	if user != nil {
		s.created = append(s.created, *user)
	}
	return s.Store.CreateUser(ctx, user)
}

// TestRegisterCreatesActiveAccount pins the contract that self-registration
// produces a usable account.
//
// Login does not check User.Active, but every endpoint behind
// auth.RequireActiveUser does. An account created with the zero value
// therefore logs in successfully, gets a session cookie, and is then rejected
// with 403 account_suspended on the first protected call — which the console
// renders as an immediate bounce back to /login.
func TestRegisterCreatesActiveAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	spy := &createUserSpy{Store: store.NewMemoryStore()}
	if err := spy.UpsertBillingPlan(context.Background(), &store.BillingPlan{
		PlanID: store.BillingPlanFree, DisplayName: "Free", Kind: "subscription",
		PackageName: "free", IncludedQuotaBytes: 5 * 1024 * 1024 * 1024, Active: true,
	}); err != nil {
		t.Fatalf("seed Free plan: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithEmailVerification(false), WithStore(spy))

	payload := map[string]string{
		"name":     "Self Registered User",
		"email":    "self-registered@example.com",
		"password": "supersecure",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d, body: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	if len(spy.created) != 1 {
		t.Fatalf("expected registration to create exactly one user, got %d", len(spy.created))
	}

	if !spy.created[0].Active {
		t.Fatal("registration persisted an inactive account: login would succeed and every protected endpoint would answer 403 account_suspended")
	}
	created, err := spy.GetUserByEmail(context.Background(), "self-registered@example.com")
	if err != nil {
		t.Fatalf("load registered account: %v", err)
	}
	profile, err := spy.GetAccountBillingProfile(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load new account billing profile: %v", err)
	}
	const freeQuota = int64(5 * 1024 * 1024 * 1024)
	if profile.PackageName != "free" || profile.IncludedQuotaBytes != freeQuota || profile.PricingRuleVersion != "plan:FREE" {
		t.Fatalf("new registration must receive catalog Free 5GiB: %+v", profile)
	}
	quota, err := spy.GetAccountQuotaState(context.Background(), created.ID)
	if err != nil || quota.RemainingIncludedQuota != freeQuota {
		t.Fatalf("new registration quota mismatch: state=%+v err=%v", quota, err)
	}
}
