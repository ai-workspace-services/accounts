package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

func seedQuotaPlan(t *testing.T, st store.Store, id, name, pkg string, quota int64, features map[string]any) {
	t.Helper()
	if err := st.UpsertBillingPlan(context.Background(), &store.BillingPlan{
		PlanID: id, DisplayName: name, Kind: "subscription", PackageName: pkg,
		IncludedQuotaBytes: quota, Features: features, Active: true,
	}); err != nil {
		t.Fatalf("seed plan %s: %v", id, err)
	}
}

func TestPublicPlanCatalogReturnsExplicitMaximumTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	const (
		fiveGiB   = int64(5 * 1024 * 1024 * 1024)
		twentyGiB = int64(20 * 1024 * 1024 * 1024)
	)
	seedQuotaPlan(t, st, "FREE", "Free", "free", fiveGiB, map[string]any{"quota_cycle": "natural_month", "fast_lane": map[string]any{"mode": "quota"}})
	seedQuotaPlan(t, st, "PLUS", "Plus", "plus", twentyGiB, map[string]any{"quota_cycle": "natural_month", "fast_lane": map[string]any{"mode": "quota"}})
	seedQuotaPlan(t, st, "UNLIMITED-BETA", "无限制（内测）", "unlimited-beta", 0, map[string]any{"quota_cycle": "none", "fast_lane": map[string]any{"mode": "unlimited"}, "internal_only": true})

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/billing/plans", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list plans status %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Plans []struct {
			PlanID             string `json:"planId"`
			IncludedQuotaBytes int64  `json:"includedQuotaBytes"`
			MaxTrafficBytes    int64  `json:"maxTrafficBytes"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode plans: %v", err)
	}
	want := map[string]int64{"FREE": fiveGiB, "PLUS": twentyGiB, "UNLIMITED-BETA": 0}
	for _, plan := range response.Plans {
		if expected, ok := want[plan.PlanID]; ok {
			if plan.IncludedQuotaBytes != expected || plan.MaxTrafficBytes != expected {
				t.Errorf("plan %s quota fields = %d/%d, want %d", plan.PlanID, plan.IncludedQuotaBytes, plan.MaxTrafficBytes, expected)
			}
			delete(want, plan.PlanID)
		}
	}
	for missing := range want {
		t.Errorf("public plan catalog omitted %s", missing)
	}
}

func TestAccountPlanContractSeparatesAssignmentFromDefaultReference(t *testing.T) {
	st := store.NewMemoryStore()
	const fiveGiB = int64(5 * 1024 * 1024 * 1024)
	seedQuotaPlan(t, st, "FREE", "Free", "free", fiveGiB, map[string]any{"quota_cycle": "natural_month", "fast_lane": map[string]any{"mode": "quota"}})
	h := &handler{store: st}

	contract, err := h.accountPlanContract(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolve unassigned plan contract: %v", err)
	}
	if contract.AssignmentStatus != "unassigned" || contract.CurrentPlan != nil {
		t.Fatalf("default reference must not appear assigned: %+v", contract)
	}
	if contract.DefaultPlan == nil || contract.DefaultPlan.PlanID != store.BillingPlanFree || contract.DefaultPlan.MaxTrafficBytes != fiveGiB || contract.DefaultPlan.Assigned {
		t.Fatalf("default Free plan must clearly expose its 5GiB limit as reference data: %+v", contract.DefaultPlan)
	}

	profile := &store.AccountBillingProfile{
		PackageName: "free", IncludedQuotaBytes: fiveGiB, PricingRuleVersion: "plan:FREE",
	}
	contract, err = h.accountPlanContract(context.Background(), profile)
	if err != nil {
		t.Fatalf("resolve assigned plan contract: %v", err)
	}
	if contract.AssignmentStatus != "assigned" || contract.CurrentPlan == nil || !contract.CurrentPlan.Assigned {
		t.Fatalf("account plan assignment not reflected: %+v", contract)
	}
	if contract.CurrentPlan.MaxTrafficBytes != fiveGiB || contract.CurrentPlan.CatalogMaxTrafficBytes != fiveGiB {
		t.Fatalf("assigned plan limit mismatch: %+v", contract.CurrentPlan)
	}
}

func TestLegacyDefaultBillingProfileRemainsUnassigned(t *testing.T) {
	st := store.NewMemoryStore()
	const freeQuota = int64(5 * 1024 * 1024 * 1024)
	seedQuotaPlan(t, st, "FREE", "Free", "free", freeQuota, map[string]any{"quota_cycle": "natural_month", "fast_lane": map[string]any{"mode": "quota"}})
	h := &handler{store: st}

	contract, err := h.accountPlanContract(context.Background(), &store.AccountBillingProfile{
		PackageName: "default", IncludedQuotaBytes: 0, PricingRuleVersion: "legacy-rule",
	})
	if err != nil {
		t.Fatalf("resolve legacy default profile: %v", err)
	}
	if contract.AssignmentStatus != "unassigned" || contract.CurrentPlan != nil {
		t.Fatalf("legacy default profile must not be relabeled as Free: %+v", contract)
	}
	if contract.DefaultPlan == nil || contract.DefaultPlan.MaxTrafficBytes != freeQuota || contract.DefaultPlan.Assigned {
		t.Fatalf("Free 5GiB should remain reference-only for this legacy account: %+v", contract.DefaultPlan)
	}
}
