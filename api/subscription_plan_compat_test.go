package api

import (
	"account/internal/store"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"strings"
	"testing"
)

func TestSpreadsheetProCustomRoundTripAndNoOp(t *testing.T) {
	router, st, token, custom := opsHarness(t)
	ctx := context.Background()
	seedAdminPlanGroupCatalog(t, st)
	custom.Groups = []string{"User", "access:dedicated-only", "node:us-only", store.MonthlyPlusQuotaLimitGroup}
	_ = st.UpdateUser(ctx, custom)
	pro := &store.User{Name: "pro-import", Email: "pro-import@example.invalid", Active: true, Role: store.RoleUser, Groups: []string{store.MonthlyFreeQuotaLimitGroup}}
	if err := st.CreateUser(ctx, pro); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*store.User{custom, pro} {
		_ = st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, PackageName: "plus", IncludedQuotaBytes: 20 << 30, PricingRuleVersion: "plan:PLUS", RegionMultiplier: 2, LineMultiplier: 3, PeakMultiplier: 4, OffPeakMultiplier: 5})
		_ = st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 17 << 30})
	}
	source := "邮箱\t用户名\t订阅开始\t订阅结束\t\t订阅周期\t月配额\t年配额\t费用\t付费状态\t备注\n" + pro.Email + "\tother-name\t2026-01-01\t2030-12-31\tpro\tmonth\t50GB\t600GB\t200元\t已付费\tmonth contract year\n" + custom.Email + "\t\t2026-01-01\t2030-12-31\tcustom\tmonth\t50GB\t\t50元\t已付费\tUS only"
	path := "/api/auth/admin/users/plan-groups/batch"
	apply := func(input, request string) {
		t.Helper()
		w := putAdminPlanGroup(t, router, token, path, gin.H{"mode": "preview", "requestId": request, "reason": "test import", "csv": input})
		if w.Code != 200 {
			t.Fatalf("preview %d %s", w.Code, w.Body.String())
		}
		var p struct {
			PreviewToken string `json:"previewToken"`
			NoChanges    bool   `json:"noChanges"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		if p.NoChanges {
			return
		}
		w = putAdminPlanGroup(t, router, token, path, gin.H{"mode": "apply", "requestId": request, "reason": "test import", "previewToken": p.PreviewToken})
		if w.Code != 200 {
			t.Fatalf("apply %d %s", w.Code, w.Body.String())
		}
	}
	apply(source, "first-import")
	for _, u := range []*store.User{custom, pro} {
		p, _ := st.GetAccountBillingProfile(ctx, u.ID)
		q, _ := st.GetAccountQuotaState(ctx, u.ID)
		if p.IncludedQuotaBytes != 50<<30 || q.RemainingIncludedQuota != 47<<30 {
			t.Fatalf("quota/usage: %+v %+v", p, q)
		}
	}
	saved, _ := st.GetUserByID(ctx, custom.ID)
	for _, g := range custom.Groups {
		if !strings.Contains(strings.Join(saved.Groups, "|"), g) {
			t.Fatalf("custom scope removed: %s", g)
		}
	}
	profile, _ := st.GetAccountBillingProfile(ctx, custom.ID)
	if profile.PackageName != "plus" || profile.RegionMultiplier != 2 || profile.LineMultiplier != 3 {
		t.Fatalf("custom pricing/scope changed: %+v", profile)
	}
	profile, _ = st.GetAccountBillingProfile(ctx, pro.ID)
	if profile.PackageName != "pro" {
		t.Fatal("pro silently became plus")
	}
	record, _ := st.GetSubscriptionRecord(ctx, custom.ID)
	if record.PlanCode != "CUSTOM" || record.SubscriptionType != "month" {
		t.Fatalf("custom record %+v", record)
	}
	exported := opsGet(t, router, token, "/api/auth/admin/users/subscription-records?format=csv&includeHistory=true")
	if exported.Code != 200 || !strings.Contains(exported.Body.String(), ",custom,") || !strings.Contains(exported.Body.String(), ",pro,") {
		t.Fatalf("export: %s", exported.Body.String())
	}
	apply(exported.Body.String(), "round-trip")
	w := putAdminPlanGroup(t, router, token, path, gin.H{"mode": "preview", "requestId": "no-op", "reason": "test import", "csv": source})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"noChanges":true`) {
		t.Fatalf("identical import not no-op: %s", w.Body.String())
	}
}

func TestImportUnknownSkipPreservesOriginalErrorRow(t *testing.T) {
	_, st, _, target := opsHarness(t)
	h := &handler{store: st}
	source, skipped, err := h.prepareSubscriptionImport(context.Background(), "email,plan\nmissing@example.invalid,plus\n"+target.Email+",invalid")
	if err != nil || len(skipped) != 1 {
		t.Fatal(err)
	}
	_, err = h.subscriptionCSVUpdates(context.Background(), source, false)
	if err == nil || !strings.Contains(err.Error(), "row 3: invalid plan") {
		t.Fatalf("wrong source row: %v", err)
	}
}
