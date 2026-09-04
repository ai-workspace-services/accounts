package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

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
	if strings.Contains(rec.Body.String(), "paused-proxy-id") {
		t.Fatalf("paused VLESS client leaked into agent config: %s", rec.Body.String())
	}
	if !user.Active {
		t.Fatal("pausing VLESS must not deactivate the account")
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
		AccountUUID        string     `json:"accountUuid"`
		TotalBytes         int64      `json:"totalBytes"`
		IncludedQuotaBytes int64      `json:"includedQuotaBytes"`
		UsedBytes          int64      `json:"usedBytes"`
		UsagePercent       float64    `json:"usagePercent"`
		PeriodStart        *time.Time `json:"periodStart"`
		PeriodEnd          *time.Time `json:"periodEnd"`
		SourceOfTruth      string     `json:"sourceOfTruth"`
		BillingProfile     struct {
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

func TestAccountUsageSummaryInitializesMissingFreeEntitlement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	if err := st.CreateUser(ctx, &store.User{
		Name: "New account", Email: "new-account@example.com", PasswordHash: "hashed",
		EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, "new-account@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: store.BillingPlanFree, DisplayName: "Free", Kind: "subscription",
		PackageName: "free", IncludedQuotaBytes: 5 * 1024 * 1024 * 1024, Active: true,
	}); err != nil {
		t.Fatalf("seed free plan: %v", err)
	}
	if err := st.CreateSession(ctx, "new-account-session", user.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))
	req := httptest.NewRequest(http.MethodGet, "/api/account/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer new-account-session")
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
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage summary: %v", err)
	}
	const freeQuota = int64(5 * 1024 * 1024 * 1024)
	if payload.IncludedQuotaBytes != freeQuota || payload.RemainingIncludedQuota != freeQuota {
		t.Fatalf("expected free quota %d, got included=%d remaining=%d", freeQuota, payload.IncludedQuotaBytes, payload.RemainingIncludedQuota)
	}
	if payload.PeriodStart == nil || payload.PeriodEnd == nil || !payload.PeriodEnd.After(*payload.PeriodStart) {
		t.Fatalf("expected a valid billing period, got start=%v end=%v", payload.PeriodStart, payload.PeriodEnd)
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
