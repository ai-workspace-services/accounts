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
	source := "邮箱\t用户名\t订阅开始\t订阅结束\t\t订阅周期\t月配额\t年配额\t费用\t付费状态\t备注\n" + pro.Email + "\tother-name\t2026-01-01\t2030-12-31\tpro\tmonth\t50GB\t600GB\t200元\t已付费\tmonth contract year\n" + custom.Email + "\t\t2026-01-01\t2030-12-31\tcustom\tmonth\t1TB\t\t50元\t已付费\tUS only"
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
		expectedQuota := int64(50 << 30)
		if u.ID == custom.ID {
			expectedQuota = 1 << 40
		}
		if p.IncludedQuotaBytes != expectedQuota || q.RemainingIncludedQuota != expectedQuota-3<<30 {
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

func TestSpreadsheetProExplicitQuotaAndJSONItemsExport(t *testing.T) {
	router, st, token, target := opsHarness(t)
	ctx := context.Background()
	seedAdminPlanGroupCatalog(t, st)
	_ = st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
		AccountUUID:        target.ID,
		PackageName:        "plus",
		IncludedQuotaBytes: 20 << 30,
		PricingRuleVersion: "plan:PLUS",
	})
	source := "邮箱,用户名,订阅开始,订阅结束,套餐,订阅类型,月配额,年配额,费用,付费状态,备注\n" +
		target.Email + ",pro-custom-quota,2026-09-01,2027-08-30,pro,month,30GB,600GB,200元,已付费,12+1赠送一个月"
	path := "/api/auth/admin/users/plan-groups/batch"
	w := putAdminPlanGroup(t, router, token, path, gin.H{
		"mode":      "preview",
		"requestId": "pro-30gb-preview",
		"reason":    "google sheets sync",
		"csv":       source,
	})
	if w.Code != 200 {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		PreviewToken string `json:"previewToken"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &preview)
	if preview.PreviewToken == "" {
		t.Fatalf("missing previewToken: %s", w.Body.String())
	}
	w = putAdminPlanGroup(t, router, token, path, gin.H{
		"mode":         "apply",
		"requestId":    "pro-30gb-preview",
		"reason":       "google sheets sync",
		"previewToken": preview.PreviewToken,
	})
	if w.Code != 200 {
		t.Fatalf("apply %d %s", w.Code, w.Body.String())
	}
	profile, err := st.GetAccountBillingProfile(ctx, target.ID)
	if err != nil || profile.PackageName != "pro" || profile.IncludedQuotaBytes != 30<<30 {
		t.Fatalf("unexpected pro profile: %+v %v", profile, err)
	}
	rec := opsGet(t, router, token, "/api/auth/admin/users/subscription-records?includeHistory=true")
	if rec.Code != 200 {
		t.Fatalf("json export: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Records []store.SubscriptionRecord `json:"records"`
		Items   []struct {
			UserID            string `json:"userId"`
			Email             string `json:"email"`
			Plan              string `json:"plan"`
			MonthlyQuotaBytes int64  `json:"monthlyQuotaBytes"`
			AnnualQuotaBytes  int64  `json:"annualQuotaBytes"`
			FeeMinor          int64  `json:"feeMinor"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode export json: %v", err)
	}
	if len(payload.Records) != 1 || len(payload.Items) != 1 {
		t.Fatalf("expected 1 record & item, got records=%d items=%d", len(payload.Records), len(payload.Items))
	}
	if payload.Items[0].Email != target.Email || payload.Items[0].Plan != "pro" || payload.Items[0].MonthlyQuotaBytes != 30<<30 || payload.Items[0].AnnualQuotaBytes != 600<<30 || payload.Items[0].FeeMinor != 20000 {
		t.Fatalf("unexpected item payload: %+v", payload.Items[0])
	}
}
