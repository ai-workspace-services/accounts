package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/routingpolicy"
	"account/internal/store"
	"account/internal/xrayconfig"
)

func seedDualBucketUser(t *testing.T, st store.Store, name, email, proxyUUID, pkg string, groups []string, vpsIncluded, vpsRem, hsIncluded, hsRem int64) *store.User {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	u := &store.User{
		Name:          name,
		Email:         email,
		PasswordHash:  "hash",
		EmailVerified: true,
		Active:        true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		ProxyUUID:     proxyUUID,
		Groups:        groups,
	}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID:         u.ID,
		PackageName:         pkg,
		IncludedQuotaBytes:  vpsIncluded,
		HighSpeedQuotaBytes: hsIncluded,
		PricingRuleVersion:  "plan:" + strings.ToUpper(pkg),
	}); err != nil {
		t.Fatalf("upsert profile %s: %v", email, err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID:             u.ID,
		RemainingIncludedQuota:  vpsRem,
		RemainingHighSpeedQuota: hsRem,
		SuspendState:            "active",
		ProxyAccessState:        "active",
		ThrottleState:           "normal",
		PeriodStart:             &start,
		PeriodEnd:               &end,
	}); err != nil {
		t.Fatalf("upsert quota %s: %v", email, err)
	}
	if err := st.CreateSession(ctx, "sess-"+proxyUUID, u.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("create session %s: %v", email, err)
	}
	return u
}

func dualBucketStatusReader(now time.Time) stubAgentStatusReader {
	return stubAgentStatusReader{
		statuses: []agentserver.StatusSnapshot{
			{
				Agent: agentserver.Identity{
					ID:     "jp-xconnect-pro-01.svc.plus",
					NodeID: "jp-xconnect-pro-01.svc.plus",
					Name:   "JP Pro EC2",
					Groups: []string{"resource:pro"},
				},
				UpdatedAt: now,
				Report: agentproto.StatusReport{
					Role:    "agent-proxy",
					Healthy: true,
					Xray: agentproto.XrayStatus{
						Running:     true,
						Region:      "jp",
						EntryPoint:  "jp-pro.example.com",
						RoutingPool: routingpolicy.RoutingPoolProEC2,
					},
				},
			},
			{
				Agent: agentserver.Identity{
					ID:     "jp-xconnect-free-01.svc.plus",
					NodeID: "jp-xconnect-free-01.svc.plus",
					Name:   "JP Media",
					Groups: []string{"resource:plus"},
				},
				UpdatedAt: now,
				Report: agentproto.StatusReport{
					Role:    "agent-proxy",
					Healthy: true,
					Xray: agentproto.XrayStatus{
						Running:     true,
						Region:      "jp",
						EntryPoint:  "jp-media.example.com",
						RoutingPool: routingpolicy.RoutingPoolMediaPriority,
					},
				},
			},
			{
				Agent: agentserver.Identity{
					ID:     "us-custom.svc.plus",
					NodeID: "us-custom.svc.plus",
					Name:   "US Custom",
					Groups: []string{"resource:custom"},
				},
				UpdatedAt: now,
				Report: agentproto.StatusReport{
					Role:    "agent-proxy",
					Healthy: true,
					Xray: agentproto.XrayStatus{
						Running:       true,
						Region:        "us",
						EntryPoint:    "us-custom.example.com",
						RoutingPool:   routingpolicy.RoutingPoolCustomDedicated,
						AllowedEmails: []string{"custom-zero@example.com"},
					},
				},
			},
		},
	}
}

func TestDualBucketProGateMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	now := time.Now().UTC()

	st := store.NewMemoryStore()
	proHSExhausted := seedDualBucketUser(t, st, "Pro HS Exhausted", "pro-hs0@example.com", "uuid-pro-hs0", "pro",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 50<<30, 40<<30, 30<<30, 0)
	proVPSExhausted := seedDualBucketUser(t, st, "Pro VPS Exhausted", "pro-vps0@example.com", "uuid-pro-vps0", "pro",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 50<<30, 0, 30<<30, 10<<30)
	proBothExhausted := seedDualBucketUser(t, st, "Pro Both Exhausted", "pro-both0@example.com", "uuid-pro-both0", "pro",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 50<<30, 0, 30<<30, 0)
	plusActive := seedDualBucketUser(t, st, "Plus Active", "plus-active@example.com", "uuid-plus-active", "plus",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 30<<30, 20<<30, 0, 0)
	plusExhausted := seedDualBucketUser(t, st, "Plus Exhausted", "plus-zero@example.com", "uuid-plus-zero", "plus",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 30<<30, 0, 0, 0)
	customZero := seedDualBucketUser(t, st, "Custom Zero", "custom-zero@example.com", "uuid-custom-zero", "custom",
		[]string{store.MonthlyPlusQuotaLimitGroup, "resource:custom"}, 50<<30, 0, 30<<30, 0)

	reader := dualBucketStatusReader(now)
	registry, err := agentserver.NewRegistry(agentserver.Config{
		Credentials: []agentserver.Credential{
			{ID: "pro-agent", Name: "Pro Agent", Token: "tok-pro", Groups: []string{"resource:pro"}},
			{ID: "plus-agent", Name: "Plus Agent", Token: "tok-plus", Groups: []string{"resource:plus"}},
			{ID: "custom-agent", Name: "Custom Agent", Token: "tok-custom", Groups: []string{"resource:custom"}},
		},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithAgentStatusReader(reader), WithEmailVerification(false))
	h := &handler{store: st, agentStatusReader: reader}

	// 1. Flag OFF (default): Pro with HS=0 & VPS=40GB still retains Pro-EC2-Pool access.
	t.Setenv("DUAL_BUCKET_PRO_GATE_ENABLED", "false")
	proClientsFlagOff, _, err := h.authorizedAgentClients(ctx, []string{"resource:pro"})
	if err != nil {
		t.Fatalf("authorizedAgentClients flag off: %v", err)
	}
	proSetOff := clientIDSet(proClientsFlagOff)
	if !proSetOff[proHSExhausted.ProxyUUID] {
		t.Fatalf("expected Pro HS-exhausted user to retain Pro-EC2-Pool when flag is false")
	}

	// 2. Flag ON: independent dual-bucket gating.
	t.Setenv("DUAL_BUCKET_PRO_GATE_ENABLED", "true")

	proClients, _, err := h.authorizedAgentClients(ctx, []string{"resource:pro"})
	if err != nil {
		t.Fatalf("authorizedAgentClients pro: %v", err)
	}
	proSet := clientIDSet(proClients)
	if proSet[proHSExhausted.ProxyUUID] {
		t.Fatalf("Pro HS=0 must be excluded from Pro-EC2-Pool agent")
	}
	if !proSet[proVPSExhausted.ProxyUUID] {
		t.Fatalf("Pro HS=10GB & VPS=0 must be retained on Pro-EC2-Pool agent")
	}
	if proSet[proBothExhausted.ProxyUUID] {
		t.Fatalf("Pro HS=0 & VPS=0 must be excluded from Pro-EC2-Pool agent")
	}
	if proSet[plusActive.ProxyUUID] || proSet[plusExhausted.ProxyUUID] {
		t.Fatalf("Plus users must never appear in Pro-EC2-Pool agent")
	}

	plusClients, _, err := h.authorizedAgentClients(ctx, []string{"resource:plus"})
	if err != nil {
		t.Fatalf("authorizedAgentClients plus: %v", err)
	}
	plusSet := clientIDSet(plusClients)
	if !plusSet[proHSExhausted.ProxyUUID] {
		t.Fatalf("Pro HS=0 & VPS=40GB must still retain Media-Pool access")
	}
	if plusSet[proVPSExhausted.ProxyUUID] {
		t.Fatalf("Pro HS=10GB & VPS=0 must be excluded from Media-Pool agent")
	}
	if !plusSet[plusActive.ProxyUUID] || plusSet[plusExhausted.ProxyUUID] {
		t.Fatalf("unexpected Plus membership in Media-Pool: active=%v zero=%v", plusSet[plusActive.ProxyUUID], plusSet[plusExhausted.ProxyUUID])
	}

	// Custom whitelist user with both buckets 0 must always pass Custom-Dedicated agent.
	if exhausted, err := h.quotaExhaustedForAgentScope(ctx, customZero, agentproto.UserScope{ResourceClass: agentResourceCustom}, []string{"resource:custom"}); err != nil || exhausted {
		t.Fatalf("Custom whitelist must never be quota-exhausted: exhausted=%v err=%v", exhausted, err)
	}
	if exhausted, err := h.quotaExhaustedForAgentScope(ctx, customZero, agentproto.UserScope{ResourceClass: agentResourceTeam, TeamID: "alpha"}, []string{"team:alpha"}); err != nil || exhausted {
		t.Fatalf("Team scope must never be quota-exhausted: exhausted=%v err=%v", exhausted, err)
	}
	customClients, _, err := h.authorizedAgentClients(ctx, []string{"resource:custom"})
	if err != nil {
		t.Fatalf("authorizedAgentClients custom: %v", err)
	}
	if !clientIDSet(customClients)[customZero.ProxyUUID] {
		t.Fatalf("Custom whitelist user with 0 buckets must remain authorized on Custom-Dedicated")
	}

	// Verify /api/agent-server/v1/nodes & /api/subscription/:format & /api/agent-server/v1/regional-pools
	// Pro HS=0 & VPS=40GB -> gets Media-Pool only, highSpeedQuotaExhausted=true, no Pro-EC2-Pool in Clash/VLESS.
	nodesRec := performAuthedGET(t, router, "/api/agent-server/v1/nodes", "sess-"+proHSExhausted.ProxyUUID)
	if nodesRec.Code != http.StatusOK {
		t.Fatalf("Pro HS=0 nodes status=%d body=%s", nodesRec.Code, nodesRec.Body.String())
	}
	var nodes []VlessNode
	if err := json.Unmarshal(nodesRec.Body.Bytes(), &nodes); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	for _, n := range nodes {
		if n.RoutingPool == routingpolicy.RoutingPoolProEC2 {
			t.Fatalf("Pro HS=0 must not receive Pro-EC2-Pool node: %+v", n)
		}
		if !n.HighSpeedQuotaExhausted {
			t.Fatalf("expected highSpeedQuotaExhausted=true on fallback node: %+v", n)
		}
	}
	if len(nodes) == 0 {
		t.Fatalf("expected Media-Pool fallback node for Pro HS=0 & VPS=40GB")
	}

	subRec := performAuthedGET(t, router, "/api/auth/sync/subscription/clash", "sess-"+proHSExhausted.ProxyUUID)
	if subRec.Code != http.StatusOK {
		t.Fatalf("Pro HS=0 clash status=%d body=%s", subRec.Code, subRec.Body.String())
	}
	if strings.Contains(subRec.Body.String(), "jp-pro.example.com") {
		t.Fatalf("Pro HS=0 clash subscription leaked Pro-EC2-Pool node: %s", subRec.Body.String())
	}
	if !strings.Contains(subRec.Body.String(), "jp-media.example.com") {
		t.Fatalf("Pro HS=0 clash subscription missing Media-Pool fallback node: %s", subRec.Body.String())
	}

	vlessRec := performAuthedGET(t, router, "/api/agent-server/v1/subscription?format=vless", "sess-"+proHSExhausted.ProxyUUID)
	if vlessRec.Code != http.StatusOK || strings.Contains(vlessRec.Body.String(), "jp-pro.example.com") || !strings.Contains(vlessRec.Body.String(), "jp-media.example.com") {
		t.Fatalf("Pro HS=0 vless subscription mismatch: status=%d body=%s", vlessRec.Code, vlessRec.Body.String())
	}

	poolsRec := performAuthedGET(t, router, "/api/agent-server/v1/regional-pools", "sess-"+proHSExhausted.ProxyUUID)
	if poolsRec.Code != http.StatusOK {
		t.Fatalf("Pro HS=0 regional-pools status=%d body=%s", poolsRec.Code, poolsRec.Body.String())
	}
	if strings.Contains(poolsRec.Body.String(), "jp-pro.example.com") || !strings.Contains(poolsRec.Body.String(), "jp-media.example.com") {
		t.Fatalf("Pro HS=0 regional-pools mismatch: %s", poolsRec.Body.String())
	}

	// Pro HS=10GB & VPS=0 -> gets Pro-EC2-Pool only, Media-Pool excluded.
	proVPS0NodesRec := performAuthedGET(t, router, "/api/agent-server/v1/nodes", "sess-"+proVPSExhausted.ProxyUUID)
	if proVPS0NodesRec.Code != http.StatusOK {
		t.Fatalf("Pro VPS=0 nodes status=%d body=%s", proVPS0NodesRec.Code, proVPS0NodesRec.Body.String())
	}
	var proVPS0Nodes []VlessNode
	if err := json.Unmarshal(proVPS0NodesRec.Body.Bytes(), &proVPS0Nodes); err != nil {
		t.Fatalf("decode proVPS0 nodes: %v", err)
	}
	if len(proVPS0Nodes) != 1 || proVPS0Nodes[0].RoutingPool != routingpolicy.RoutingPoolProEC2 {
		t.Fatalf("Pro HS=10GB & VPS=0 expected only Pro-EC2-Pool node, got %+v", proVPS0Nodes)
	}

	// Pro HS=0 & VPS=0 -> 403 quota_exhausted.
	if rec := performAuthedGET(t, router, "/api/agent-server/v1/nodes", "sess-"+proBothExhausted.ProxyUUID); rec.Code != http.StatusForbidden {
		t.Fatalf("Pro both=0 expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Plus active & Plus exhausted -> never see Pro-EC2-Pool; Plus exhausted gets 403.
	plusNodesRec := performAuthedGET(t, router, "/api/agent-server/v1/nodes", "sess-"+plusActive.ProxyUUID)
	if plusNodesRec.Code != http.StatusOK || strings.Contains(plusNodesRec.Body.String(), "jp-pro.example.com") {
		t.Fatalf("Plus active leaked Pro-EC2-Pool or failed: status=%d body=%s", plusNodesRec.Code, plusNodesRec.Body.String())
	}
	if rec := performAuthedGET(t, router, "/api/agent-server/v1/nodes", "sess-"+plusExhausted.ProxyUUID); rec.Code != http.StatusForbidden {
		t.Fatalf("Plus VPS=0 expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountUsageSummaryDualBucketAndPoolBreakdown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	now := time.Now().UTC()
	st := store.NewMemoryStore()

	proPlan := &store.BillingPlan{
		PlanID:              "PRO-MONTHLY-V2",
		DisplayName:         "Pro 月付",
		Kind:                "subscription",
		IncludedQuotaBytes:  50 << 30,
		HighSpeedQuotaBytes: 30 << 30,
		PackageName:         "pro",
		PriceAmount:         5000,
		PriceCurrency:       "CNY",
		PriceUnit:           "month",
		Active:              true,
	}
	if err := st.UpsertBillingPlan(ctx, proPlan); err != nil {
		t.Fatalf("upsert pro plan: %v", err)
	}
	freePlan := &store.BillingPlan{
		PlanID:             store.BillingPlanFree,
		DisplayName:        "Free",
		Kind:               "subscription",
		IncludedQuotaBytes: 5 << 30,
		PackageName:        "free",
		Active:             true,
	}
	if err := st.UpsertBillingPlan(ctx, freePlan); err != nil {
		t.Fatalf("upsert free plan: %v", err)
	}

	user := seedDualBucketUser(t, st, "Pro Summary", "pro-summary@example.com", "uuid-pro-summary", "pro",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 50<<30, 40<<30, 30<<30, 0)
	// Ensure PricingRuleVersion points to PRO-MONTHLY-V2
	p, _ := st.GetAccountBillingProfile(ctx, user.ID)
	p.PricingRuleVersion = "plan:PRO-MONTHLY-V2"
	if err := st.UpsertAccountBillingProfile(ctx, p); err != nil {
		t.Fatalf("update profile: %v", err)
	}

	// Seed buckets across explicit RoutingPool, node snapshot metadata fallback, and unknown fallback.
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   now.Add(-10 * time.Minute),
		NodeID:        "prod:jp-xconnect-pro-01.svc.plus",
		AccountUUID:   user.ID,
		RoutingPool:   routingpolicy.RoutingPoolProEC2,
		UplinkBytes:   10 << 30,
		DownlinkBytes: 20 << 30,
		TotalBytes:    30 << 30,
	}); err != nil {
		t.Fatalf("bucket pro: %v", err)
	}
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{
		BucketStart:   now.Add(-5 * time.Minute),
		NodeID:        "prod:jp-xconnect-01.svc.plus",
		AccountUUID:   user.ID,
		RoutingPool:   "", // empty routingPool -> falls back to Media-Pool
		UplinkBytes:   2 << 30,
		DownlinkBytes: 8 << 30,
		TotalBytes:    10 << 30,
	}); err != nil {
		t.Fatalf("bucket media: %v", err)
	}

	t.Setenv("DUAL_BUCKET_PRO_GATE_ENABLED", "true")
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	rec := performAuthedGET(t, router, "/api/account/usage/summary", "sess-"+user.ProxyUUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage summary status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		// Legacy fields preserved
		AccountUUID            string  `json:"accountUuid"`
		TotalBytes             int64   `json:"totalBytes"`
		UplinkBytes            int64   `json:"uplinkBytes"`
		DownlinkBytes          int64   `json:"downlinkBytes"`
		RemainingIncludedQuota int64   `json:"remainingIncludedQuota"`
		IncludedQuotaBytes     int64   `json:"includedQuotaBytes"`
		UsedBytes              int64   `json:"usedBytes"`
		UsagePercent           float64 `json:"usagePercent"`
		QuotaExhausted         bool    `json:"quotaExhausted"`
		NetworkAccessState     string  `json:"networkAccessState"`
		// New dual-bucket fields
		HighSpeedQuotaBytes     int64   `json:"highSpeedQuotaBytes"`
		RemainingHighSpeedQuota int64   `json:"remainingHighSpeedQuota"`
		HighSpeedUsedBytes      int64   `json:"highSpeedUsedBytes"`
		HighSpeedUsagePercent   float64 `json:"highSpeedUsagePercent"`
		HighSpeedQuotaExhausted bool    `json:"highSpeedQuotaExhausted"`
		VPSQuotaBytes           int64   `json:"vpsQuotaBytes"`
		RemainingVPSQuota       int64   `json:"remainingVpsQuota"`
		VPSUsedBytes            int64   `json:"vpsUsedBytes"`
		VPSUsagePercent         float64 `json:"vpsUsagePercent"`
		VPSQuotaExhausted       bool    `json:"vpsQuotaExhausted"`
		PoolUsageBreakdown      map[string]struct {
			UplinkBytes   int64 `json:"uplinkBytes"`
			DownlinkBytes int64 `json:"downlinkBytes"`
			TotalBytes    int64 `json:"totalBytes"`
		} `json:"poolUsageBreakdown"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode usage summary: %v", err)
	}
	if resp.AccountUUID != user.ID || resp.TotalBytes != 40<<30 || resp.UplinkBytes != 12<<30 || resp.DownlinkBytes != 28<<30 {
		t.Fatalf("legacy totals mismatch: %+v", resp)
	}
	if resp.IncludedQuotaBytes != 50<<30 || resp.RemainingIncludedQuota != 40<<30 || resp.UsedBytes != 10<<30 || resp.QuotaExhausted {
		t.Fatalf("legacy VPS quota mismatch: %+v", resp)
	}
	if resp.NetworkAccessState != "active" {
		t.Fatalf("expected networkAccessState=active when VPS remains, got %q", resp.NetworkAccessState)
	}
	if resp.HighSpeedQuotaBytes != 30<<30 || resp.RemainingHighSpeedQuota != 0 || resp.HighSpeedUsedBytes != 30<<30 || resp.HighSpeedUsagePercent != 100 || !resp.HighSpeedQuotaExhausted {
		t.Fatalf("high-speed summary mismatch: %+v", resp)
	}
	if resp.VPSQuotaBytes != 50<<30 || resp.RemainingVPSQuota != 40<<30 || resp.VPSUsedBytes != 10<<30 || resp.VPSUsagePercent != 20 || resp.VPSQuotaExhausted {
		t.Fatalf("vps summary mismatch: %+v", resp)
	}
	proPool, ok := resp.PoolUsageBreakdown[routingpolicy.RoutingPoolProEC2]
	if !ok || proPool.TotalBytes != 30<<30 || proPool.UplinkBytes != 10<<30 || proPool.DownlinkBytes != 20<<30 {
		t.Fatalf("Pro-EC2-Pool breakdown mismatch: %+v", resp.PoolUsageBreakdown)
	}
	mediaPool, ok := resp.PoolUsageBreakdown[routingpolicy.RoutingPoolMediaPriority]
	if !ok || mediaPool.TotalBytes != 10<<30 || mediaPool.UplinkBytes != 2<<30 || mediaPool.DownlinkBytes != 8<<30 {
		t.Fatalf("Media-Pool breakdown mismatch: %+v", resp.PoolUsageBreakdown)
	}
}

func TestDualBucketEntitlementDowngradeHelpers(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	h := &handler{store: st}

	proPlan := &store.BillingPlan{
		PlanID:              "PRO-MONTHLY-V2",
		Kind:                "subscription",
		IncludedQuotaBytes:  50 << 30,
		HighSpeedQuotaBytes: 30 << 30,
		PackageName:         "pro",
		Active:              true,
	}
	plusPlan := &store.BillingPlan{
		PlanID:              store.BillingPlanPlus,
		Kind:                "subscription",
		IncludedQuotaBytes:  30 << 30,
		HighSpeedQuotaBytes: 0,
		PackageName:         "plus",
		Active:              true,
	}
	user := seedDualBucketUser(t, st, "Downgrade User", "downgrade@example.com", "uuid-downgrade", "pro",
		[]string{store.MonthlyPlusQuotaLimitGroup}, 50<<30, 45<<30, 30<<30, 20<<30)

	if err := h.resetQuotaForPlan(ctx, user.ID, proPlan); err != nil {
		t.Fatalf("resetQuotaForPlan: %v", err)
	}
	q, _ := st.GetAccountQuotaState(ctx, user.ID)
	if q.RemainingIncludedQuota != 50<<30 || q.RemainingHighSpeedQuota != 30<<30 {
		t.Fatalf("reset Pro buckets = %d/%d", q.RemainingIncludedQuota, q.RemainingHighSpeedQuota)
	}

	// Downgrade Pro -> Plus immediately zeroes HighSpeedQuotaBytes & RemainingHighSpeedQuota.
	if err := h.applyPlanEntitlements(ctx, user.ID, plusPlan); err != nil {
		t.Fatalf("applyPlanEntitlements plus: %v", err)
	}
	if err := h.clampDowngradedHighSpeedQuota(ctx, user.ID, plusPlan); err != nil {
		t.Fatalf("clampDowngradedHighSpeedQuota: %v", err)
	}
	p, _ := st.GetAccountBillingProfile(ctx, user.ID)
	q, _ = st.GetAccountQuotaState(ctx, user.ID)
	if p.HighSpeedQuotaBytes != 0 || q.RemainingHighSpeedQuota != 0 {
		t.Fatalf("expected high-speed zeroed after Pro->Plus downgrade, got %d/%d", p.HighSpeedQuotaBytes, q.RemainingHighSpeedQuota)
	}
	if err := h.downgradeToFreePlan(ctx, user.ID); err != nil {
		t.Fatalf("downgradeToFreePlan: %v", err)
	}
	p, _ = st.GetAccountBillingProfile(ctx, user.ID)
	q, _ = st.GetAccountQuotaState(ctx, user.ID)
	if p.HighSpeedQuotaBytes != 0 || q.RemainingHighSpeedQuota != 0 {
		t.Fatalf("expected high-speed zeroed after downgradeToFreePlan, got %d/%d", p.HighSpeedQuotaBytes, q.RemainingHighSpeedQuota)
	}
}

func performAuthedGET(t *testing.T, router http.Handler, path, sessionToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func clientIDSet(clients []xrayconfig.Client) map[string]bool {
	out := make(map[string]bool, len(clients))
	for _, c := range clients {
		out[c.ID] = true
	}
	return out
}
