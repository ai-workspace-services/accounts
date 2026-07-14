package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
)

const testStripeWebhookSecret = "whsec_test_secret"

// billingEventRecorder is implemented by the memory store, standing in for
// the PGMQ billing_events queue in unit tests.
type billingEventRecorder interface {
	BillingEventsForTest() []store.BillingEvent
}

func billingEventTypes(t *testing.T, st store.Store) []string {
	t.Helper()
	recorder, ok := st.(billingEventRecorder)
	if !ok {
		t.Fatalf("store does not record billing events")
	}
	events := recorder.BillingEventsForTest()
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func signStripePayload(t *testing.T, payload []byte) string {
	t.Helper()
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(testStripeWebhookSecret))
	_, _ = mac.Write([]byte(timestamp + "." + string(payload)))
	return fmt.Sprintf("t=%s,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

func newBillingWebhookHarness(t *testing.T) (*gin.Engine, store.Store, *store.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{
		Name:          "Billing User",
		Email:         "billing@example.com",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID:             "PRO-M",
		StripePriceID:      "price_pro_month",
		DisplayName:        "Pro Monthly",
		Kind:               "subscription",
		IncludedQuotaBytes: 100 << 30,
		PackageName:        "pro",
		PriceMultipliers:   map[string]float64{"region": 1.5},
		Active:             true,
	}); err != nil {
		t.Fatalf("seed pro plan: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID:             store.BillingPlanFree,
		DisplayName:        "Free",
		Kind:               "subscription",
		PackageName:        "default",
		IncludedQuotaBytes: 0,
		Active:             true,
	}); err != nil {
		t.Fatalf("seed free plan: %v", err)
	}

	router := gin.New()
	RegisterRoutes(
		router,
		WithStore(st),
		WithStripeConfig(StripeConfig{SecretKey: "sk_test_x", WebhookSecret: testStripeWebhookSecret}),
	)
	return router, st, user
}

func postStripeEvent(t *testing.T, router *gin.Engine, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook", bytes.NewReader(payload))
	req.Header.Set("Stripe-Signature", signStripePayload(t, payload))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func subscriptionEventPayload(eventID, eventType, userID, priceID, status string) []byte {
	return []byte(fmt.Sprintf(`{
		"id": %q,
		"type": %q,
		"data": {"object": {
			"id": "sub_123",
			"status": %q,
			"customer": "cus_123",
			"metadata": {"user_id": %q, "plan_id": "PRO-M"},
			"current_period_start": 1750000000,
			"current_period_end": 1752600000,
			"items": {"data": [{"price": {"id": %q}}]}
		}}
	}`, eventID, eventType, status, userID, priceID))
}

func TestStripeSubscriptionCreatedSyncsEntitlementsAndDedups(t *testing.T) {
	router, st, user := newBillingWebhookHarness(t)
	ctx := context.Background()

	// Live trial that must be superseded by the paid subscription.
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "trial", Kind: "trial", PlanID: store.BillingPlanTrial7D,
		ExternalID: "trial-" + user.ID, Status: "active",
	}); err != nil {
		t.Fatalf("seed trial: %v", err)
	}

	payload := subscriptionEventPayload("evt_1", "customer.subscription.created", user.ID, "price_pro_month", "active")
	if rec := postStripeEvent(t, router, payload); rec.Code != http.StatusOK {
		t.Fatalf("webhook failed: %d %s", rec.Code, rec.Body.String())
	}

	profile, err := st.GetAccountBillingProfile(ctx, user.ID)
	if err != nil || profile == nil {
		t.Fatalf("expected billing profile, err=%v", err)
	}
	if profile.PackageName != "pro" || profile.IncludedQuotaBytes != 100<<30 {
		t.Fatalf("unexpected profile: %+v", profile)
	}
	if profile.RegionMultiplier != 1.5 || profile.LineMultiplier != 1.0 {
		t.Fatalf("unexpected multipliers: %+v", profile)
	}
	if profile.PricingRuleVersion != "plan:PRO-M" {
		t.Fatalf("unexpected pricing rule version %q", profile.PricingRuleVersion)
	}

	quota, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil || quota == nil {
		t.Fatalf("expected quota state, err=%v", err)
	}
	if quota.RemainingIncludedQuota != 100<<30 || quota.Arrears || quota.SuspendState != "active" {
		t.Fatalf("unexpected quota state: %+v", quota)
	}

	subs, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	trialSuperseded := false
	for _, sub := range subs {
		if sub.Kind == "trial" && sub.Status == "superseded" {
			trialSuperseded = true
		}
	}
	if !trialSuperseded {
		t.Fatalf("expected trial to be superseded, got %+v", subs)
	}

	// Replay the exact event: acknowledged as duplicate, no side effects.
	profile.PackageName = "tampered"
	if err := st.UpsertAccountBillingProfile(ctx, profile); err != nil {
		t.Fatalf("tamper profile: %v", err)
	}
	eventsBefore := billingEventTypes(t, st)
	foundActivated := false
	for _, typ := range eventsBefore {
		if typ == "subscription_activated" {
			foundActivated = true
		}
	}
	if !foundActivated {
		t.Fatalf("expected subscription_activated billing event, got %v", eventsBefore)
	}

	rec := postStripeEvent(t, router, payload)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("expected duplicate ack, got %d %s", rec.Code, rec.Body.String())
	}
	if after := billingEventTypes(t, st); len(after) != len(eventsBefore) {
		t.Fatalf("replay published extra billing events: before=%v after=%v", eventsBefore, after)
	}
	after, err := st.GetAccountBillingProfile(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	if after.PackageName != "tampered" {
		t.Fatalf("replayed event mutated state: %+v", after)
	}
}

func TestStripeSubscriptionDeletedDowngradesToFree(t *testing.T) {
	router, st, user := newBillingWebhookHarness(t)
	ctx := context.Background()

	created := subscriptionEventPayload("evt_create", "customer.subscription.created", user.ID, "price_pro_month", "active")
	if rec := postStripeEvent(t, router, created); rec.Code != http.StatusOK {
		t.Fatalf("create webhook failed: %d %s", rec.Code, rec.Body.String())
	}

	deleted := subscriptionEventPayload("evt_delete", "customer.subscription.deleted", user.ID, "price_pro_month", "canceled")
	if rec := postStripeEvent(t, router, deleted); rec.Code != http.StatusOK {
		t.Fatalf("delete webhook failed: %d %s", rec.Code, rec.Body.String())
	}

	profile, err := st.GetAccountBillingProfile(ctx, user.ID)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if profile.PackageName != "default" || profile.IncludedQuotaBytes != 0 {
		t.Fatalf("expected free downgrade, got %+v", profile)
	}
	quota, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil {
		t.Fatalf("load quota: %v", err)
	}
	if quota.RemainingIncludedQuota != 0 {
		t.Fatalf("expected zeroed quota, got %+v", quota)
	}
}

func TestMarkArrearsAndQuotaReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{store: st}

	if err := h.markAccountArrears(ctx, "user-1"); err != nil {
		t.Fatalf("mark arrears: %v", err)
	}
	quota, err := st.GetAccountQuotaState(ctx, "user-1")
	if err != nil || !quota.Arrears {
		t.Fatalf("expected arrears flag, err=%v state=%+v", err, quota)
	}

	plan := &store.BillingPlan{PlanID: "PRO-M", IncludedQuotaBytes: 42, PackageName: "pro"}
	if err := h.resetQuotaForPlan(ctx, "user-1", plan); err != nil {
		t.Fatalf("reset quota: %v", err)
	}
	quota, err = st.GetAccountQuotaState(ctx, "user-1")
	if err != nil {
		t.Fatalf("load quota: %v", err)
	}
	if quota.Arrears || quota.RemainingIncludedQuota != 42 || quota.ThrottleState != "normal" || quota.SuspendState != "active" {
		t.Fatalf("unexpected quota after reset: %+v", quota)
	}
}

func TestValidCheckoutPricePrefersCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{
		store:  st,
		stripe: newStripeClient(StripeConfig{SecretKey: "sk_test_x", AllowedPriceIDs: []string{"price_env_only"}}),
	}

	// Bootstrap mode: empty catalog falls back to the env allowlist.
	if !h.validCheckoutPrice(ctx, "price_env_only") {
		t.Fatalf("expected env allowlist fallback to accept price_env_only")
	}
	if h.validCheckoutPrice(ctx, "price_unknown") {
		t.Fatalf("expected env allowlist to reject unknown price")
	}

	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: "PRO-M", StripePriceID: "price_catalog", Kind: "subscription", Active: true,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if !h.validCheckoutPrice(ctx, "price_catalog") {
		t.Fatalf("expected catalog price to be purchasable")
	}
	// Catalog now authoritative: env-only price is no longer purchasable.
	if h.validCheckoutPrice(ctx, "price_env_only") {
		t.Fatalf("expected catalog to override env allowlist")
	}

	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: "PRO-M", StripePriceID: "price_catalog", Kind: "subscription", Active: false,
	}); err != nil {
		t.Fatalf("deactivate plan: %v", err)
	}
	if h.validCheckoutPrice(ctx, "price_catalog") {
		t.Fatalf("expected inactive plan price to be rejected")
	}
}

func TestPublicAndAdminBillingPlanEndpoints(t *testing.T) {
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

	router := gin.New()
	RegisterRoutes(router, WithStore(st))

	// Admin upsert.
	body := `{"displayName":"Pro Monthly","kind":"subscription","includedQuotaBytes":1024,"packageName":"pro","stripePriceId":"price_x","active":true,"sortOrder":5}`
	req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/billing/plans/pro-m", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin upsert failed: %d %s", rec.Code, rec.Body.String())
	}

	// Inactive plan must not appear publicly.
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "HIDDEN", Kind: "subscription", Active: false}); err != nil {
		t.Fatalf("seed hidden plan: %v", err)
	}

	pubReq := httptest.NewRequest(http.MethodGet, "/api/billing/plans", nil)
	pubRec := httptest.NewRecorder()
	router.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("public listing failed: %d %s", pubRec.Code, pubRec.Body.String())
	}
	var pub struct {
		Plans []billingPlanPayload `json:"plans"`
	}
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode public plans: %v", err)
	}
	if len(pub.Plans) != 1 || pub.Plans[0].PlanID != "PRO-M" {
		t.Fatalf("unexpected public plans: %+v", pub.Plans)
	}

	// Invalid kind rejected.
	badReq := httptest.NewRequest(http.MethodPut, "/api/auth/admin/billing/plans/bad", strings.NewReader(`{"kind":"nonsense"}`))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("Authorization", "Bearer "+adminToken)
	badRec := httptest.NewRecorder()
	router.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid kind rejection, got %d", badRec.Code)
	}

	// Delete.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/auth/admin/billing/plans/pro-m", nil)
	delReq.Header.Set("Authorization", "Bearer "+adminToken)
	delRec := httptest.NewRecorder()
	router.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("admin delete failed: %d %s", delRec.Code, delRec.Body.String())
	}

	// Unauthenticated admin access rejected.
	anonReq := httptest.NewRequest(http.MethodGet, "/api/auth/admin/billing/plans", nil)
	anonRec := httptest.NewRecorder()
	router.ServeHTTP(anonRec, anonReq)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous admin listing, got %d", anonRec.Code)
	}
}

// TestOAuthDefersTrialUntilEmailVerified asserts the gated onboarding flow:
// an OAuth signup alone does NOT grant the trial or its entitlements — the
// user must complete our own email-verification round trip first, at which
// point the TRIAL-7D subscription, billing profile and quota are provisioned.
func TestOAuthDefersTrialUntilEmailVerified(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	memStore := store.NewMemoryStore()
	if err := memStore.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID:             store.BillingPlanTrial7D,
		Kind:               "trial",
		IncludedQuotaBytes: 10 << 30,
		PackageName:        "trial",
		TrialDays:          7,
		Active:             true,
	}); err != nil {
		t.Fatalf("seed trial plan: %v", err)
	}

	mailer := &testEmailSender{}
	router := gin.New()
	RegisterRoutes(
		router,
		WithStore(memStore),
		WithEmailSender(mailer),
		WithOAuthProviders(map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{profile: &auth.OAuthUserProfile{
				ID:       "oauth-trial-1",
				Email:    "trial-oauth@example.com",
				Name:     "Trial User",
				Verified: true,
			}},
		}),
		WithOAuthFrontendURL("https://console.svc.plus"),
	)

	// Step 1: OAuth callback — creates the user, but withholds the trial.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oauth/callback/github?code=test-code", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("oauth callback failed: %d %s", rec.Code, rec.Body.String())
	}

	user, err := memStore.GetUserByEmail(ctx, "trial-oauth@example.com")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if user.EmailVerified {
		t.Fatalf("expected OAuth user to start unverified")
	}
	if bp, err := memStore.GetAccountBillingProfile(ctx, user.ID); err == nil && bp != nil {
		t.Fatalf("trial entitlements must not be provisioned before verification, got %+v", bp)
	}

	// Step 2: send + verify the email code — this activates the trial.
	sendBody, _ := json.Marshal(map[string]string{"email": "trial-oauth@example.com"})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(sendBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send verification failed: %d %s", rec.Code, rec.Body.String())
	}
	msg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email")
	}
	code := extractVerificationCodeFromMessage(t, msg)

	verifyBody, _ := json.Marshal(map[string]string{"email": "trial-oauth@example.com", "code": code})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify failed: %d %s", rec.Code, rec.Body.String())
	}

	// Step 3: trial entitlements are now in place.
	verified, err := memStore.GetUserByEmail(ctx, "trial-oauth@example.com")
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if !verified.EmailVerified {
		t.Fatalf("expected user to be verified after code entry")
	}
	bp, err := memStore.GetAccountBillingProfile(ctx, user.ID)
	if err != nil || bp.PackageName != "trial" || bp.IncludedQuotaBytes != 10<<30 {
		t.Fatalf("expected trial entitlements after verification, err=%v profile=%+v", err, bp)
	}
	quota, err := memStore.GetAccountQuotaState(ctx, user.ID)
	if err != nil || quota.RemainingIncludedQuota != 10<<30 {
		t.Fatalf("expected trial quota after verification, err=%v quota=%+v", err, quota)
	}
}
