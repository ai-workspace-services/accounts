package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestSubscriptionCSVEmailUpsertPreviewReplayAndExport(t *testing.T) {
	router, st, _, target := opsHarness(t)
	ctx := context.Background()
	root := &store.User{Name: "root-import", Email: "root-import@example.invalid", Role: store.RoleRoot, Active: true, EmailVerified: true}
	if err := st.CreateUser(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "root-csv", root.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	teamUser := &store.User{Name: "team-csv", Email: "team-csv@example.invalid", Role: store.RoleUser, Groups: []string{"team:alpha", "team-admin:alpha", store.MonthlyPlusQuotaLimitGroup}}
	if err := st.CreateUser(ctx, teamUser); err != nil {
		t.Fatal(err)
	}
	seedAdminPlanGroupCatalog(t, st)
	_ = st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: target.ID, PackageName: "plus", IncludedQuotaBytes: 20 * adminPlanTestGiB, PricingRuleVersion: "plan:PLUS"})
	_ = st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: target.ID, RemainingIncludedQuota: 17 * adminPlanTestGiB, EffectiveAt: time.Now()})
	source := "邮箱,用户名,订阅开始,订阅结束,订阅类型,月配额,年配额,费用,付费状态,备注\n TARGET@EXAMPLE.COM ,other-name,2026.8.1,2030.7.30,year,20GB,240GB,200元,已付费,\"赠送一个月,包含备注\n第二行\"\n"
	path := "/api/auth/admin/users/plan-groups/batch"
	apply := func(csv, requestID string) {
		t.Helper()
		preview := putAdminPlanGroup(t, router, "root-csv", path, gin.H{"mode": "preview", "requestId": requestID, "reason": "csv-test", "csv": csv, "reconcile": true})
		if preview.Code != 200 {
			t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
		}
		var body struct {
			PreviewToken string `json:"previewToken"`
		}
		_ = json.Unmarshal(preview.Body.Bytes(), &body)
		if requestID == "first" {
			if rows, _ := st.ListSubscriptionRecords(ctx); len(rows) != 0 {
				t.Fatal("preview persisted subscription")
			}
		}
		for range 2 {
			response := putAdminPlanGroup(t, router, "root-csv", path, gin.H{"mode": "apply", "requestId": requestID, "reason": "csv-test", "previewToken": body.PreviewToken})
			if response.Code != 200 {
				t.Fatalf("apply: %d %s", response.Code, response.Body.String())
			}
		}
	}
	apply(source, "first")
	apply(source, "second-upload")
	records, err := st.ListSubscriptionRecords(ctx)
	if err != nil || len(records) != 2 {
		t.Fatalf("one row per non-root personal account: %d %v", len(records), err)
	}
	record, err := st.GetSubscriptionRecord(ctx, target.ID)
	if err != nil || record.FeeMinor != 20000 || record.AnnualQuotaBytes != 240*adminPlanTestGiB || record.ValidFrom.Format("2006-01-02") != "2026-08-01" || !strings.Contains(record.Note, "第二行") {
		t.Fatalf("fields not persisted: %+v %v", record, err)
	}
	teamStored, _ := st.GetUserByID(ctx, teamUser.ID)
	if store.MonthlyQuotaGroup(teamStored) != store.MonthlyPlusQuotaLimitGroup {
		t.Fatal("personal import modified team user")
	}
	if _, err := st.GetSubscriptionRecord(ctx, teamUser.ID); err == nil {
		t.Fatal("personal import wrote a team record")
	}
	quota, _ := st.GetAccountQuotaState(ctx, target.ID)
	if quota.RemainingIncludedQuota != 17*adminPlanTestGiB {
		t.Fatal("repeated import reset usage")
	}
	exported := opsGet(t, router, "root-csv", "/api/auth/admin/users/subscription-records?format=csv")
	if exported.Code != 200 || !strings.Contains(exported.Body.String(), "fee_minor") {
		t.Fatalf("export: %d %s", exported.Code, exported.Body.String())
	}
	apply(exported.Body.String(), "round-trip")
	updated := strings.Replace(source, "200元", "220元", 1)
	apply(updated, "update-existing-email")
	record, _ = st.GetSubscriptionRecord(ctx, target.ID)
	if record.FeeMinor != 22000 {
		t.Fatal("email update did not persist new fee")
	}
	details := opsGet(t, router, "root-csv", "/api/auth/admin/billing/accounts/"+target.ID)
	if details.Code != 200 || !strings.Contains(details.Body.String(), `"feeMinor":22000`) || !strings.Contains(details.Body.String(), `"remainingIncludedQuota":18253611008`) {
		t.Fatalf("details don't reflect database: %d %s", details.Code, details.Body.String())
	}
	// No fallback to username, no ambiguous duplicate rows, and no silent free downgrade on malformed files.
	for i, bad := range []string{
		"email,username,paid status\nwrong@example.invalid,target,paid",
		"email,paid status\ntarget@example.com,paid\nTARGET@example.com,paid",
		"email,subscription start\ntarget@example.com,2026.2.30",
		"email,monthly quota\ntarget@example.com,NaN",
	} {
		response := putAdminPlanGroup(t, router, "root-csv", path, gin.H{"mode": "preview", "requestId": fmt.Sprint("bad", i), "reason": "csv-test", "csv": bad, "reconcile": true})
		if response.Code != 400 {
			t.Fatalf("bad csv accepted: %d %s", response.Code, response.Body.String())
		}
	}
	// A payment status containing the substring 'paid' must not be treated as paid.
	handler := &handler{store: st}
	unpaid, err := handler.subscriptionCSVUpdates(ctx, "email,paid status\ntarget@example.com,unpaid", false)
	if err != nil || len(unpaid) != 1 || unpaid[0].PlanID != store.BillingPlanFree {
		t.Fatal("unpaid row granted paid access")
	}
}

func TestSubscriptionCSVRequiresRoot(t *testing.T) {
	router, _, token, _ := opsHarness(t)
	response := putAdminPlanGroup(t, router, token, "/api/auth/admin/users/plan-groups/batch", gin.H{"mode": "preview", "requestId": "not-root", "reason": "test", "csv": "email\ntarget@example.com"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-root import allowed: %d", response.Code)
	}
	if rec := opsGet(t, router, token, "/api/auth/admin/users/subscription-records?format=csv"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-root export allowed: %d", rec.Code)
	}
}
