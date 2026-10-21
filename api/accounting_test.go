package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"account/internal/agentserver"
	"account/internal/store"
)

func TestAgentUsersUseAccountEmailAsStatsEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)

	st := store.NewMemoryStore()
	ctx := context.Background()

	if err := st.CreateUser(ctx, &store.User{
		Name:          "Stats User",
		Email:         "stats@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
		ProxyUUID:     "proxy-user-id",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	user, err := st.GetUserByEmail(ctx, "stats@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	registry, err := agentserver.NewRegistry(agentserver.Config{
		Credentials: []agentserver.Credential{{
			ID:    "*",
			Name:  "test-agent",
			Token: "agent-token",
		}},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/users", nil)
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("X-Agent-ID", "hk-xhttp.svc.plus")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
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

	for _, client := range payload.Clients {
		if client.Email == strings.ToLower(strings.TrimSpace(user.Email)) {
			return
		}
	}

	t.Fatalf("expected stats email %q in payload, got %#v", user.Email, payload.Clients)
}

func TestAgentUsersExcludeOperatorPausedVLESSAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{Name: "Paused VLESS", Email: "paused-vless@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "paused-proxy-id"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, "paused-vless@example.com")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: user.ID, ProxyAccessState: "paused", SuspendState: "active", ThrottleState: "normal"}); err != nil {
		t.Fatalf("pause vless: %v", err)
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "*", Name: "test-agent", Token: "agent-token"}}})
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
		t.Fatalf("agent users: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("agent users response must expose a revision ETag for event-driven sync")
	}
	if strings.Contains(rec.Body.String(), "paused-proxy-id") {
		t.Fatalf("paused VLESS client leaked into agent config: %s", rec.Body.String())
	}
	if !user.Active {
		t.Fatal("pausing VLESS must not deactivate the account")
	}
}

func TestAgentUsersWithholdExhaustedQuotaWithoutDeletingAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	ctx := context.Background()

	exhausted := &store.User{Name: "Exhausted", Email: "exhausted@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "exhausted-proxy-id", Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	plusExhausted := &store.User{Name: "Plus Exhausted", Email: "plus-exhausted@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "plus-exhausted-proxy-id", Groups: []string{store.MonthlyPlusQuotaLimitGroup}}
	internalBeta := &store.User{Name: "Internal Beta", Email: "internal-beta@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "internal-beta-proxy-id", Groups: []string{store.MonthlyUnlimitedBetaQuotaGroup}}
	remaining := &store.User{Name: "Remaining", Email: "remaining@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "remaining-proxy-id"}
	unselected := &store.User{Name: "Unselected", Email: "unselected@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "unselected-proxy-id"}
	legacy := &store.User{Name: "Legacy", Email: "legacy@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: true, ProxyUUID: "legacy-proxy-id"}
	unverified := &store.User{Name: "Unverified", Email: "unverified@example.com", PasswordHash: "hashed", Role: store.RoleUser, Active: true, ProxyUUID: "unverified-proxy-id", Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	inactive := &store.User{Name: "Inactive", Email: "inactive@example.com", PasswordHash: "hashed", EmailVerified: true, Role: store.RoleUser, Active: false, ProxyUUID: "inactive-proxy-id"}
	for _, user := range []*store.User{exhausted, plusExhausted, internalBeta, remaining, unselected, legacy, unverified, inactive} {
		if err := st.CreateUser(ctx, user); err != nil {
			t.Fatalf("create user %s: %v", user.Email, err)
		}
	}
	// CreateUser defaults new accounts to active; explicitly disable this fixture.
	inactive.Active = false
	if err := st.UpdateUser(ctx, inactive); err != nil {
		t.Fatalf("disable fixture: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: exhausted.ID, RemainingIncludedQuota: 0,
		SuspendState: "active", ProxyAccessState: "active", ThrottleState: "normal",
	}); err != nil {
		t.Fatalf("set exhausted quota: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID: exhausted.ID, PackageName: "free", IncludedQuotaBytes: 3 * 1024 * 1024 * 1024,
	}); err != nil {
		t.Fatalf("set exhausted billing profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: plusExhausted.ID, RemainingIncludedQuota: 0,
		SuspendState: "active", ProxyAccessState: "active", ThrottleState: "normal",
	}); err != nil {
		t.Fatalf("set plus exhausted quota: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID: plusExhausted.ID, PackageName: "pro", IncludedQuotaBytes: 20 * 1024 * 1024 * 1024,
	}); err != nil {
		t.Fatalf("set plus exhausted billing profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: internalBeta.ID, RemainingIncludedQuota: 0,
		SuspendState: "active", ProxyAccessState: "active", ThrottleState: "normal",
	}); err != nil {
		t.Fatalf("set internal beta quota: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID: internalBeta.ID, PackageName: "free", IncludedQuotaBytes: 3 * 1024 * 1024 * 1024,
	}); err != nil {
		t.Fatalf("set internal beta billing profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: unselected.ID, RemainingIncludedQuota: 0,
		SuspendState: "active", ProxyAccessState: "active", ThrottleState: "normal",
	}); err != nil {
		t.Fatalf("set unselected quota: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID: unselected.ID, PackageName: "free", IncludedQuotaBytes: 3 * 1024 * 1024 * 1024,
	}); err != nil {
		t.Fatalf("set unselected billing profile: %v", err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: remaining.ID, RemainingIncludedQuota: 1,
		SuspendState: "active", ProxyAccessState: "active", ThrottleState: "normal",
	}); err != nil {
		t.Fatalf("set remaining quota: %v", err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID: remaining.ID, PackageName: "default", IncludedQuotaBytes: 1024,
	}); err != nil {
		t.Fatalf("set remaining billing profile: %v", err)
	}
	// Operator pause/resume can create a quota-state row for a legacy account.
	// Without a billing profile, its zero value is not a monthly quota grant.
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: legacy.ID, RemainingIncludedQuota: 0,
		SuspendState: "active", ProxyAccessState: "active", ThrottleState: "normal",
	}); err != nil {
		t.Fatalf("set legacy proxy state: %v", err)
	}

	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "*", Name: "test-agent", Token: "agent-token"}}})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithEmailVerification(true))
	req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/users", nil)
	req.Header.Set("Authorization", "Bearer agent-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent users: %d %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, unverified.ProxyUUID) || strings.Contains(body, inactive.ProxyUUID) {
		t.Fatalf("unverified or inactive client leaked into agent config: %s", body)
	}
	if strings.Contains(body, exhausted.ProxyUUID) {
		t.Fatalf("quota-exhausted free client leaked into agent config: %s", body)
	}
	if strings.Contains(body, plusExhausted.ProxyUUID) {
		t.Fatalf("quota-exhausted plus client without override leaked into agent config: %s", body)
	}
	if !strings.Contains(body, internalBeta.ProxyUUID) {
		t.Fatalf("unlimited internal beta client was removed: %s", body)
	}
	if !strings.Contains(body, remaining.ProxyUUID) {
		t.Fatalf("client with remaining quota was removed: %s", body)
	}
	if !strings.Contains(body, unselected.ProxyUUID) {
		t.Fatalf("unselected free client was removed: %s", body)
	}
	if !strings.Contains(body, legacy.ProxyUUID) {
		t.Fatalf("legacy client without billing profile was removed: %s", body)
	}

	if err := st.CreateSession(ctx, "exhausted-session", exhausted.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create exhausted session: %v", err)
	}
	usageReq := httptest.NewRequest(http.MethodGet, "/api/account/usage/summary", nil)
	usageReq.Header.Set("Authorization", "Bearer exhausted-session")
	usageRec := httptest.NewRecorder()
	router.ServeHTTP(usageRec, usageReq)
	if usageRec.Code != http.StatusOK {
		t.Fatalf("usage summary: %d %s", usageRec.Code, usageRec.Body.String())
	}
	var access struct {
		QuotaExhausted      bool   `json:"quotaExhausted"`
		IncludedQuotaBytes  int64  `json:"includedQuotaBytes"`
		RemainingQuota      int64  `json:"remainingIncludedQuota"`
		UsedBytes           int64  `json:"usedBytes"`
		NetworkAccessState  string `json:"networkAccessState"`
		NetworkAccessReason string `json:"networkAccessReason"`
	}
	if err := json.Unmarshal(usageRec.Body.Bytes(), &access); err != nil {
		t.Fatalf("decode access state: %v", err)
	}
	if !access.QuotaExhausted || access.NetworkAccessState != "paused" || access.NetworkAccessReason != "quota_exhausted" {
		t.Fatalf("unexpected exhausted access state: %+v", access)
	}
	if access.IncludedQuotaBytes != 3<<30 || access.RemainingQuota != 0 || access.UsedBytes != 3<<30 {
		t.Fatalf("quota statistics must remain intact: %+v", access)
	}
	if persisted, err := st.GetUserByID(ctx, exhausted.ID); err != nil || persisted.Email != exhausted.Email {
		t.Fatalf("quota pause must retain the user record: user=%+v err=%v", persisted, err)
	}
}

func TestAccountUsageAndPolicyEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	st := store.NewMemoryStore()
	ctx := context.Background()

	if err := st.CreateUser(ctx, &store.User{
		Name:          "Billing User",
		Email:         "billing@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	user, err := st.GetUserByEmail(ctx, "billing@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	sessionToken := "usage-session-token"
	if err := st.CreateSession(ctx, sessionToken, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	bucketStart := time.Date(2026, 4, 1, 10, 30, 0, 0, time.UTC)
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   bucketStart,
		NodeID:        "node-a",
		AccountUUID:   user.ID,
		Region:        "hk",
		LineCode:      "premium",
		UplinkBytes:   128,
		DownlinkBytes: 256,
		TotalBytes:    384,
		Multiplier:    1.5,
		RatingStatus:  store.RatingStatusRated,
	}); err != nil {
		t.Fatalf("upsert bucket: %v", err)
	}
	// A second Xray node may report the same user UUID. Accounts must expose
	// one account-level total while Billing keeps independent node checkpoints.
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   bucketStart,
		NodeID:        "node-b",
		AccountUUID:   user.ID,
		Region:        "sg",
		UplinkBytes:   40,
		DownlinkBytes: 60,
		TotalBytes:    100,
		Multiplier:    1.0,
		RatingStatus:  store.RatingStatusRated,
	}); err != nil {
		t.Fatalf("upsert second-node bucket: %v", err)
	}

	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID:            user.ID,
		RemainingIncludedQuota: 2048,
		CurrentBalance:         87.5,
		Arrears:                false,
		ThrottleState:          "normal",
		SuspendState:           "active",
		PeriodStart:            &periodStart,
		PeriodEnd:              &periodEnd,
		EffectiveAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert quota state: %v", err)
	}

	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID:        user.ID,
		PackageName:        "starter",
		IncludedQuotaBytes: 4096,
		BasePricePerByte:   0.125,
		RegionMultiplier:   1.2,
		LineMultiplier:     1.5,
		PeakMultiplier:     1.0,
		OffPeakMultiplier:  1.0,
		PricingRuleVersion: "pricing-v1",
	}); err != nil {
		t.Fatalf("upsert billing profile: %v", err)
	}

	if err := st.InsertBillingLedgerEntry(ctx, &store.BillingLedgerEntry{
		ID:                 "ledger-1",
		AccountUUID:        user.ID,
		BucketStart:        bucketStart,
		BucketEnd:          bucketStart.Add(time.Minute),
		EntryType:          "traffic_charge",
		RatedBytes:         384,
		AmountDelta:        -1.25,
		BalanceAfter:       87.5,
		PricingRuleVersion: "pricing-v1",
	}); err != nil {
		t.Fatalf("insert billing ledger: %v", err)
	}

	if err := st.UpsertAccountPolicySnapshot(ctx, &store.AccountPolicySnapshot{
		AccountUUID:        user.ID,
		PolicyVersion:      "policy-v1",
		AuthState:          "active",
		RateProfile:        "standard",
		ConnProfile:        "standard",
		EligibleNodeGroups: []string{"hk-premium"},
		PreferredStrategy:  "ewma",
		DegradeMode:        "fallback",
		ExpiresAt:          time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/account/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("usage summary status: %d body=%s", rec.Code, rec.Body.String())
	}

	var usagePayload struct {
		AccountUUID         string     `json:"accountUuid"`
		TotalBytes          int64      `json:"totalBytes"`
		IncludedQuotaBytes  int64      `json:"includedQuotaBytes"`
		UsedBytes           int64      `json:"usedBytes"`
		UsagePercent        float64    `json:"usagePercent"`
		PeriodStart         *time.Time `json:"periodStart"`
		PeriodEnd           *time.Time `json:"periodEnd"`
		SourceOfTruth       string     `json:"sourceOfTruth"`
		QuotaExhausted      bool       `json:"quotaExhausted"`
		NetworkAccessState  string     `json:"networkAccessState"`
		NetworkAccessReason string     `json:"networkAccessReason"`
		BillingProfile      struct {
			PackageName        string  `json:"packageName"`
			BasePricePerByte   float64 `json:"basePricePerByte"`
			RegionMultiplier   float64 `json:"regionMultiplier"`
			LineMultiplier     float64 `json:"lineMultiplier"`
			PricingRuleVersion string  `json:"pricingRuleVersion"`
		} `json:"billingProfile"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &usagePayload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if usagePayload.AccountUUID != user.ID {
		t.Fatalf("expected account uuid %q, got %q", user.ID, usagePayload.AccountUUID)
	}
	if usagePayload.TotalBytes != 484 {
		t.Fatalf("expected total bytes across nodes 484, got %d", usagePayload.TotalBytes)
	}
	if usagePayload.IncludedQuotaBytes != 4096 || usagePayload.UsedBytes != 2048 || usagePayload.UsagePercent != 50 {
		t.Fatalf("unexpected quota summary: %+v", usagePayload)
	}
	if usagePayload.QuotaExhausted || usagePayload.NetworkAccessState != "active" || usagePayload.NetworkAccessReason != "" {
		t.Fatalf("expected active network access, got %+v", usagePayload)
	}
	if usagePayload.PeriodStart == nil || !usagePayload.PeriodStart.Equal(periodStart) || usagePayload.PeriodEnd == nil || !usagePayload.PeriodEnd.Equal(periodEnd) {
		t.Fatalf("unexpected quota period: start=%v end=%v", usagePayload.PeriodStart, usagePayload.PeriodEnd)
	}
	if usagePayload.SourceOfTruth != "postgresql" {
		t.Fatalf("expected source of truth postgresql, got %q", usagePayload.SourceOfTruth)
	}
	if usagePayload.BillingProfile.PackageName != "starter" {
		t.Fatalf("expected billing profile package starter, got %q", usagePayload.BillingProfile.PackageName)
	}

	bucketsReq := httptest.NewRequest(http.MethodGet, "/api/account/usage/buckets", nil)
	bucketsReq.Header.Set("Authorization", "Bearer "+sessionToken)
	bucketsRec := httptest.NewRecorder()
	router.ServeHTTP(bucketsRec, bucketsReq)

	if bucketsRec.Code != http.StatusOK {
		t.Fatalf("usage buckets status: %d body=%s", bucketsRec.Code, bucketsRec.Body.String())
	}

	var bucketsPayload struct {
		AccountUUID   string `json:"accountUuid"`
		SourceOfTruth string `json:"sourceOfTruth"`
		Buckets       []struct {
			TotalBytes  int64     `json:"totalBytes"`
			NodeID      string    `json:"nodeId"`
			BucketStart time.Time `json:"bucketStart"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(bucketsRec.Body.Bytes(), &bucketsPayload); err != nil {
		t.Fatalf("decode usage buckets payload: %v", err)
	}
	if bucketsPayload.AccountUUID != user.ID {
		t.Fatalf("expected usage buckets account uuid %q, got %q", user.ID, bucketsPayload.AccountUUID)
	}
	if bucketsPayload.SourceOfTruth != "postgresql" {
		t.Fatalf("expected usage buckets source of truth postgresql, got %q", bucketsPayload.SourceOfTruth)
	}
	if len(bucketsPayload.Buckets) != 2 {
		t.Fatalf("expected 2 per-node usage buckets, got %d", len(bucketsPayload.Buckets))
	}
	byNode := make(map[string]int64, len(bucketsPayload.Buckets))
	for _, bucket := range bucketsPayload.Buckets {
		byNode[bucket.NodeID] = bucket.TotalBytes
	}
	if byNode["node-a"] != 384 || byNode["node-b"] != 100 {
		t.Fatalf("unexpected per-node usage buckets: %#v", byNode)
	}

	billingReq := httptest.NewRequest(http.MethodGet, "/api/account/billing/summary", nil)
	billingReq.Header.Set("Authorization", "Bearer "+sessionToken)
	billingRec := httptest.NewRecorder()
	router.ServeHTTP(billingRec, billingReq)

	if billingRec.Code != http.StatusOK {
		t.Fatalf("billing summary status: %d body=%s", billingRec.Code, billingRec.Body.String())
	}

	var billingPayload struct {
		AccountUUID   string `json:"accountUuid"`
		SourceOfTruth string `json:"sourceOfTruth"`
		QuotaState    struct {
			CurrentBalance float64 `json:"currentBalance"`
		} `json:"quotaState"`
		BillingProfile struct {
			PackageName        string  `json:"packageName"`
			IncludedQuotaBytes int64   `json:"includedQuotaBytes"`
			BasePricePerByte   float64 `json:"basePricePerByte"`
		} `json:"billingProfile"`
		Ledger []struct {
			ID          string  `json:"id"`
			EntryType   string  `json:"entryType"`
			RatedBytes  int64   `json:"ratedBytes"`
			AmountDelta float64 `json:"amountDelta"`
		} `json:"ledger"`
	}
	if err := json.Unmarshal(billingRec.Body.Bytes(), &billingPayload); err != nil {
		t.Fatalf("decode billing payload: %v", err)
	}
	if billingPayload.AccountUUID != user.ID {
		t.Fatalf("expected billing account uuid %q, got %q", user.ID, billingPayload.AccountUUID)
	}
	if billingPayload.SourceOfTruth != "postgresql" {
		t.Fatalf("expected billing source of truth postgresql, got %q", billingPayload.SourceOfTruth)
	}
	if billingPayload.BillingProfile.IncludedQuotaBytes != 4096 {
		t.Fatalf("expected billing profile included quota 4096, got %d", billingPayload.BillingProfile.IncludedQuotaBytes)
	}
	if billingPayload.QuotaState.CurrentBalance != 87.5 {
		t.Fatalf("expected billing current balance 87.5, got %v", billingPayload.QuotaState.CurrentBalance)
	}
	if len(billingPayload.Ledger) != 1 {
		t.Fatalf("expected 1 billing ledger entry, got %d", len(billingPayload.Ledger))
	}
	if billingPayload.Ledger[0].ID != "ledger-1" {
		t.Fatalf("expected billing ledger id ledger-1, got %q", billingPayload.Ledger[0].ID)
	}
	if billingPayload.Ledger[0].EntryType != "traffic_charge" {
		t.Fatalf("expected billing entry type traffic_charge, got %q", billingPayload.Ledger[0].EntryType)
	}
	if billingPayload.Ledger[0].RatedBytes != 384 {
		t.Fatalf("expected billing rated bytes 384, got %d", billingPayload.Ledger[0].RatedBytes)
	}

	policyReq := httptest.NewRequest(http.MethodGet, "/api/account/policy", nil)
	policyReq.Header.Set("Authorization", "Bearer "+sessionToken)
	policyRec := httptest.NewRecorder()
	router.ServeHTTP(policyRec, policyReq)

	if policyRec.Code != http.StatusOK {
		t.Fatalf("policy status: %d body=%s", policyRec.Code, policyRec.Body.String())
	}

	var policyPayload struct {
		AccountUUID        string   `json:"accountUuid"`
		PreferredStrategy  string   `json:"preferredStrategy"`
		EligibleNodeGroups []string `json:"eligibleNodeGroups"`
	}
	if err := json.Unmarshal(policyRec.Body.Bytes(), &policyPayload); err != nil {
		t.Fatalf("decode policy payload: %v", err)
	}
	if policyPayload.AccountUUID != user.ID {
		t.Fatalf("expected policy account uuid %q, got %q", user.ID, policyPayload.AccountUUID)
	}
	if policyPayload.PreferredStrategy != "ewma" {
		t.Fatalf("expected preferred strategy ewma, got %q", policyPayload.PreferredStrategy)
	}
	if len(policyPayload.EligibleNodeGroups) != 1 || policyPayload.EligibleNodeGroups[0] != "hk-premium" {
		t.Fatalf("unexpected eligible node groups %#v", policyPayload.EligibleNodeGroups)
	}
}

func TestAccountUsageSummaryKeepsLegacyAccountUnassigned(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	if err := st.CreateUser(ctx, &store.User{
		Name: "Legacy account", Email: "legacy-account@example.com", PasswordHash: "hashed",
		EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true,
		Groups: []string{"User", store.MonthlyPlusQuotaLimitGroup},
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, "legacy-account@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: store.BillingPlanFree, DisplayName: "Free", Kind: "subscription",
		PackageName: "free", IncludedQuotaBytes: 3 * 1024 * 1024 * 1024, Active: true,
	}); err != nil {
		t.Fatalf("seed free plan: %v", err)
	}
	if err := st.CreateSession(ctx, "legacy-account-session", user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))
	req := httptest.NewRequest(http.MethodGet, "/api/account/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer legacy-account-session")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage summary status: %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		IncludedQuotaBytes     int64      `json:"includedQuotaBytes"`
		RemainingIncludedQuota int64      `json:"remainingIncludedQuota"`
		PeriodStart            *time.Time `json:"periodStart"`
		PeriodEnd              *time.Time `json:"periodEnd"`
		PlanAssignmentStatus   string     `json:"planAssignmentStatus"`
		CurrentPlan            *struct {
			Assigned        bool  `json:"assigned"`
			MaxTrafficBytes int64 `json:"maxTrafficBytes"`
		} `json:"currentPlan"`
		DefaultPlan *struct {
			PlanID          string `json:"planId"`
			MaxTrafficBytes int64  `json:"maxTrafficBytes"`
			Assigned        bool   `json:"assigned"`
		} `json:"defaultPlan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage summary: %v", err)
	}
	const freeQuota = int64(3 * 1024 * 1024 * 1024)
	if payload.IncludedQuotaBytes != 0 || payload.RemainingIncludedQuota != 0 {
		t.Fatalf("legacy account must remain unassigned, got included=%d remaining=%d", payload.IncludedQuotaBytes, payload.RemainingIncludedQuota)
	}
	if payload.PeriodStart != nil || payload.PeriodEnd != nil {
		t.Fatalf("legacy account must not receive a quota period, got start=%v end=%v", payload.PeriodStart, payload.PeriodEnd)
	}
	if payload.PlanAssignmentStatus != "unassigned" || payload.CurrentPlan != nil {
		t.Fatalf("expected an unassigned legacy account, status=%q current=%+v", payload.PlanAssignmentStatus, payload.CurrentPlan)
	}
	if payload.DefaultPlan == nil || payload.DefaultPlan.PlanID != store.BillingPlanFree || payload.DefaultPlan.MaxTrafficBytes != freeQuota || payload.DefaultPlan.Assigned {
		t.Fatalf("expected Free 3GiB to be reference-only default plan, got %+v", payload.DefaultPlan)
	}
	if _, err := st.GetAccountBillingProfile(ctx, user.ID); err == nil {
		t.Fatal("usage GET must not create a billing profile for a legacy account")
	}
	if _, err := st.GetAccountQuotaState(ctx, user.ID); err == nil {
		t.Fatal("usage GET must not create a quota state for a legacy account")
	}

	billingReq := httptest.NewRequest(http.MethodGet, "/api/account/billing/summary", nil)
	billingReq.Header.Set("Authorization", "Bearer legacy-account-session")
	billingRec := httptest.NewRecorder()
	router.ServeHTTP(billingRec, billingReq)
	if billingRec.Code != http.StatusOK {
		t.Fatalf("billing summary status: %d body=%s", billingRec.Code, billingRec.Body.String())
	}
	if _, err := st.GetAccountBillingProfile(ctx, user.ID); err == nil {
		t.Fatal("billing summary GET must not create a billing profile for a legacy account")
	}
	afterRead, err := st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload legacy account: %v", err)
	}
	if len(afterRead.Groups) != 2 || afterRead.Groups[1] != store.MonthlyPlusQuotaLimitGroup {
		t.Fatalf("read-only account summaries changed existing groups: %#v", afterRead.Groups)
	}
}

func TestInternalNetworkIdentitiesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Setenv("INTERNAL_SERVICE_TOKEN", "test-internal-token")

	st := store.NewMemoryStore()
	ctx := context.Background()

	if err := st.CreateUser(ctx, &store.User{
		Name:          "Exporter User",
		Email:         "exporter@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
		ProxyUUID:     "proxy-exporter-id",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := st.CreateUser(ctx, &store.User{
		Name:          "Inactive User",
		Email:         "inactive@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        false,
		ProxyUUID:     "proxy-inactive-id",
	}); err != nil {
		t.Fatalf("create inactive user: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/internal/network/identities", nil)
	req.Header.Set("X-Service-Token", os.Getenv("INTERNAL_SERVICE_TOKEN"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("internal identities status: %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Identities []struct {
			UUID        string `json:"uuid"`
			Email       string `json:"email"`
			AccountUUID string `json:"accountUuid"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode internal identities payload: %v", err)
	}

	foundExporter := false
	for _, identity := range payload.Identities {
		if identity.Email != "exporter@example.com" {
			continue
		}
		foundExporter = true
		if identity.UUID == "" {
			t.Fatalf("expected exporter UUID to be populated")
		}
		if identity.AccountUUID == "" {
			t.Fatalf("expected account uuid to be populated")
		}
	}
	if !foundExporter {
		t.Fatalf("expected exporter identity in payload, got %#v", payload.Identities)
	}
}

type failingAccountingStore struct {
	store.Store
	quotaCalls int
	quotaErr   error
	bucketsErr error
	profileErr error
}

func (f *failingAccountingStore) GetAccountQuotaState(ctx context.Context, accountUUID string) (*store.AccountQuotaState, error) {
	f.quotaCalls++
	if f.quotaErr != nil && f.quotaCalls > 1 {
		return nil, f.quotaErr
	}
	return f.Store.GetAccountQuotaState(ctx, accountUUID)
}

func (f *failingAccountingStore) ListTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time) ([]store.TrafficMinuteBucket, error) {
	if f.bucketsErr != nil {
		return nil, f.bucketsErr
	}
	return f.Store.ListTrafficMinuteBucketsByAccount(ctx, accountUUID, start, end)
}

func (f *failingAccountingStore) GetAccountBillingProfile(ctx context.Context, accountUUID string) (*store.AccountBillingProfile, error) {
	if f.profileErr != nil {
		return nil, f.profileErr
	}
	return f.Store.GetAccountBillingProfile(ctx, accountUUID)
}

func TestAccountUsageAndIdentitiesOTelTracePropagationAndRedaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "secret-internal-service-token-999")

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})

	st := store.NewMemoryStore()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		Name:          "Trace User",
		Email:         "trace-sensitive@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
		ProxyUUID:     "proxy-sensitive-uuid-777",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, "trace-sensitive@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	sessionToken := "session-secret-token-888"
	if err := st.CreateSession(ctx, sessionToken, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   now.Add(-2 * time.Minute),
		NodeID:        "node-hk",
		AccountUUID:   user.ID,
		Region:        "hk",
		LineCode:      "default",
		UplinkBytes:   100,
		DownlinkBytes: 200,
		TotalBytes:    300,
		Multiplier:    1.0,
		RatingStatus:  store.RatingStatusRated,
	}); err != nil {
		t.Fatalf("upsert bucket: %v", err)
	}

	router := gin.New()
	router.Use(otelgin.Middleware("web-saas-accounts"))
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const traceparent = "00-" + traceID + "-00f067aa0ba902b7-01"

	endpoints := []struct {
		path       string
		headerName string
		headerVal  string
		spanName   string
	}{
		{
			path:       "/api/account/usage/summary",
			headerName: "Authorization",
			headerVal:  "Bearer " + sessionToken,
			spanName:   "accounts.usage.summary",
		},
		{
			path:       "/api/account/usage/buckets?start=" + now.Add(-10*time.Minute).Format(time.RFC3339) + "&end=" + now.Format(time.RFC3339),
			headerName: "Authorization",
			headerVal:  "Bearer " + sessionToken,
			spanName:   "accounts.usage.buckets",
		},
		{
			path:       "/api/account/billing/summary",
			headerName: "Authorization",
			headerVal:  "Bearer " + sessionToken,
			spanName:   "accounts.billing.summary",
		},
		{
			path:       "/api/internal/network/identities",
			headerName: "X-Service-Token",
			headerVal:  "secret-internal-service-token-999",
			spanName:   "accounts.network.identities",
		},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(http.MethodGet, ep.path, nil)
		req.Header.Set("traceparent", traceparent)
		req.Header.Set(ep.headerName, ep.headerVal)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("endpoint %s returned %d: %s", ep.path, rec.Code, rec.Body.String())
		}
	}

	spans := recorder.Ended()
	foundSpans := make(map[string]bool)
	forbiddenSubstrings := []string{
		user.ID,
		user.Email,
		user.ProxyUUID,
		sessionToken,
		"secret-internal-service-token-999",
	}

	for _, sp := range spans {
		foundSpans[sp.Name()] = true
		if gotTraceID := sp.SpanContext().TraceID().String(); gotTraceID != traceID {
			t.Fatalf("span %s trace_id = %s, want %s", sp.Name(), gotTraceID, traceID)
		}
		for _, attr := range sp.Attributes() {
			val := attr.Value.Emit()
			for _, forbidden := range forbiddenSubstrings {
				if forbidden != "" && strings.Contains(val, forbidden) {
					t.Fatalf("span %s attribute %s leaked sensitive value %q in %q", sp.Name(), attr.Key, forbidden, val)
				}
			}
		}
		for _, forbidden := range forbiddenSubstrings {
			if forbidden != "" && strings.Contains(sp.Status().Description, forbidden) {
				t.Fatalf("span %s status description leaked sensitive value %q", sp.Name(), forbidden)
			}
		}
	}

	for _, ep := range endpoints {
		if !foundSpans[ep.spanName] {
			t.Fatalf("expected child span %q to be recorded, got %v", ep.spanName, foundSpans)
		}
	}
}

func TestAccountUsageWindowFilteringFreshnessAndDBErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	st := store.NewMemoryStore()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.User{
		Name:          "Window User",
		Email:         "window@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, "window@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	sessionToken := "window-session"
	if err := st.CreateSession(ctx, sessionToken, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	// 1. No buckets -> freshnessState == "no_data"
	req := httptest.NewRequest(http.MethodGet, "/api/account/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status: %d body=%s", rec.Code, rec.Body.String())
	}
	var summary struct {
		FreshnessState string `json:"freshnessState"`
		TotalBytes     int64  `json:"totalBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.FreshnessState != "no_data" {
		t.Fatalf("expected freshnessState no_data, got %q", summary.FreshnessState)
	}

	// 2. Stale bucket (> 300s ago) -> freshnessState == "stale"
	t1 := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   t1,
		NodeID:        "node-1",
		AccountUUID:   user.ID,
		UplinkBytes:   50,
		DownlinkBytes: 50,
		TotalBytes:    100,
		RatingStatus:  store.RatingStatusRated,
	}); err != nil {
		t.Fatalf("upsert t1: %v", err)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.FreshnessState != "stale" {
		t.Fatalf("expected freshnessState stale, got %q", summary.FreshnessState)
	}

	// 3. Fresh bucket (<= 300s ago) -> freshnessState == "fresh"
	t2 := time.Now().UTC().Add(-1 * time.Minute).Truncate(time.Minute)
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   t2,
		NodeID:        "node-1",
		AccountUUID:   user.ID,
		UplinkBytes:   150,
		DownlinkBytes: 150,
		TotalBytes:    300,
		RatingStatus:  store.RatingStatusRated,
	}); err != nil {
		t.Fatalf("upsert t2: %v", err)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.FreshnessState != "fresh" || summary.TotalBytes != 400 {
		t.Fatalf("expected freshnessState fresh and totalBytes 400, got %+v", summary)
	}

	// 4. UTC window query on /api/account/usage/buckets
	bucketsURL := "/api/account/usage/buckets?start=" + t2.Add(-30*time.Second).Format(time.RFC3339) + "&end=" + t2.Add(30*time.Second).Format(time.RFC3339)
	bReq := httptest.NewRequest(http.MethodGet, bucketsURL, nil)
	bReq.Header.Set("Authorization", "Bearer "+sessionToken)
	bRec := httptest.NewRecorder()
	router.ServeHTTP(bRec, bReq)
	if bRec.Code != http.StatusOK {
		t.Fatalf("buckets status: %d", bRec.Code)
	}
	var bPayload struct {
		Buckets []struct {
			TotalBytes int64 `json:"totalBytes"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(bRec.Body.Bytes(), &bPayload); err != nil {
		t.Fatalf("decode buckets: %v", err)
	}
	if len(bPayload.Buckets) != 1 || bPayload.Buckets[0].TotalBytes != 300 {
		t.Fatalf("expected 1 filtered bucket with 300 bytes, got %+v", bPayload.Buckets)
	}

	// 5. DB error on GetAccountQuotaState returns 500 instead of silently returning 0 quota
	failQuotaRouter := gin.New()
	RegisterRoutes(failQuotaRouter, WithStore(&failingAccountingStore{
		Store:    st,
		quotaErr: errors.New("simulated db connection timeout"),
	}), WithEmailVerification(false))

	qRec := httptest.NewRecorder()
	failQuotaRouter.ServeHTTP(qRec, req)
	if qRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when GetAccountQuotaState fails with DB error, got %d body=%s", qRec.Code, qRec.Body.String())
	}

	// 6. DB error on ListTrafficMinuteBucketsByAccount returns 500
	failBucketsRouter := gin.New()
	RegisterRoutes(failBucketsRouter, WithStore(&failingAccountingStore{
		Store:      st,
		bucketsErr: errors.New("simulated db buckets error"),
	}), WithEmailVerification(false))

	fbRec := httptest.NewRecorder()
	failBucketsRouter.ServeHTTP(fbRec, req)
	if fbRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when ListTrafficMinuteBucketsByAccount fails, got %d body=%s", fbRec.Code, fbRec.Body.String())
	}
}

type delayedAccountingStore struct {
	store.Store
	dbDelay time.Duration
}

func (d *delayedAccountingStore) ListTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time) ([]store.TrafficMinuteBucket, error) {
	ctx, dbSpan := otel.Tracer("otelsql").Start(ctx, "sql.conn.query")
	if d.dbDelay > 0 {
		time.Sleep(d.dbDelay)
	}
	res, err := d.Store.ListTrafficMinuteBucketsByAccount(ctx, accountUUID, start, end)
	dbSpan.End()
	return res, err
}

func TestSlowServiceVsSlowDatabaseIndependentSpanClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		_ = tp.Shutdown(context.Background())
	})

	baseStore := store.NewMemoryStore()
	ctx := context.Background()
	if err := baseStore.CreateUser(ctx, &store.User{
		Name:          "Latency User",
		Email:         "latency@example.com",
		PasswordHash:  "hashed",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := baseStore.GetUserByEmail(ctx, "latency@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	sessionToken := "latency-session"
	if err := baseStore.CreateSession(ctx, sessionToken, user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	const iterations = 20
	const injectedDelay = 12 * time.Millisecond

	// Scenario A: 20 requests with slow application middleware (+12ms) and fast DB (0ms)
	slowServiceStore := &delayedAccountingStore{Store: baseStore, dbDelay: 0}
	slowServiceRouter := gin.New()
	slowServiceRouter.Use(otelgin.Middleware("web-saas-accounts", otelgin.WithTracerProvider(tp)))
	slowServiceRouter.Use(func(c *gin.Context) {
		time.Sleep(injectedDelay)
		c.Next()
	})
	RegisterRoutes(slowServiceRouter, WithStore(slowServiceStore), WithEmailVerification(false))

	for i := 0; i < iterations; i++ {
		recorder.Reset()
		req := httptest.NewRequest(http.MethodGet, "/api/account/usage/buckets", nil)
		req.Header.Set("Authorization", "Bearer "+sessionToken)
		rec := httptest.NewRecorder()
		slowServiceRouter.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("slow service iter %d status=%d", i, rec.Code)
		}
		var httpDur, dbDur time.Duration
		for _, sp := range recorder.Ended() {
			switch sp.Name() {
			case "/api/account/usage/buckets", "GET /api/account/usage/buckets":
				httpDur = sp.EndTime().Sub(sp.StartTime())
			case "sql.conn.query":
				dbDur = sp.EndTime().Sub(sp.StartTime())
			}
		}
		if httpDur < injectedDelay || dbDur >= injectedDelay/2 {
			t.Fatalf("iter %d slow-service misclassified: httpDur=%v dbDur=%v", i, httpDur, dbDur)
		}
	}

	// Scenario B: 20 requests with fast application middleware (0ms) and slow DB (+12ms)
	slowDBStore := &delayedAccountingStore{Store: baseStore, dbDelay: injectedDelay}
	slowDBRouter := gin.New()
	slowDBRouter.Use(otelgin.Middleware("web-saas-accounts", otelgin.WithTracerProvider(tp)))
	RegisterRoutes(slowDBRouter, WithStore(slowDBStore), WithEmailVerification(false))

	for i := 0; i < iterations; i++ {
		recorder.Reset()
		req := httptest.NewRequest(http.MethodGet, "/api/account/usage/buckets", nil)
		req.Header.Set("Authorization", "Bearer "+sessionToken)
		rec := httptest.NewRecorder()
		slowDBRouter.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("slow db iter %d status=%d", i, rec.Code)
		}
		var usageDur, dbDur time.Duration
		for _, sp := range recorder.Ended() {
			switch sp.Name() {
			case "accounts.usage.buckets":
				usageDur = sp.EndTime().Sub(sp.StartTime())
			case "sql.conn.query":
				dbDur = sp.EndTime().Sub(sp.StartTime())
			}
		}
		nonDBAppTime := usageDur - dbDur
		if dbDur < injectedDelay || nonDBAppTime >= injectedDelay/2 {
			t.Fatalf("iter %d slow-db misclassified: usageDur=%v dbDur=%v nonDBAppTime=%v", i, usageDur, dbDur, nonDBAppTime)
		}
	}
}
