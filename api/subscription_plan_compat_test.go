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

func TestSpreadsheet14RowRaggedTSVAndNewTrialSubscriberImport(t *testing.T) {
	router, st, token, target := opsHarness(t)
	ctx := context.Background()
	seedAdminPlanGroupCatalog(t, st)

	// Replicate the exact production 14-row roster:
	// - 10 plus users (row 1..10)
	// - 1 pro user with explicit 30GB/600GB quota (row 11)
	// - 2 custom US-dedicated users without team:* groups (rows 12..13)
	// - 1 newly registered trial user (`buoudidi@outlook.com`) with 5 GiB trial quota and partial usage (row 14)
	type seedSpec struct {
		email  string
		groups []string
		pkg    string
		quota  int64
		rule   string
	}
	seeds := []seedSpec{
		{email: "yangzhiyun724@126.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "13191446515@163.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "challen66@163.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "sukhadukkha@163.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "208746734@qq.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "304235059@qq.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "1324171503@qq.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "514916831@qq.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "1767425958@qq.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "sccxboy@163.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "plus", quota: 30 << 30, rule: "plan:PLUS"},
		{email: "156405189@qq.com", groups: []string{"User", "segment:quota:plus-20gb"}, pkg: "pro", quota: 50 << 30, rule: "plan:PRO-MONTHLY-V2"},
		{email: "ho_yatphone@163.com", groups: []string{"User", "access:dedicated-only"}, pkg: "custom", quota: 1 << 40, rule: "plan:CUSTOM"},
		{email: "3667108906@qq.com", groups: []string{"User"}, pkg: "custom", quota: 1 << 40, rule: "plan:CUSTOM"},
		{email: "buoudidi@outlook.com", groups: []string{"User", store.OneTimeTrialQuotaGroup}, pkg: "trial", quota: 5 << 30, rule: "plan:TRIAL-5GB-ONCE"},
	}
	userIDByEmail := map[string]string{target.Email: target.ID}
	for _, s := range seeds {
		u := &store.User{
			Name:   strings.Split(s.email, "@")[0],
			Email:  s.email,
			Active: true,
			Role:   store.RoleUser,
			Groups: s.groups,
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("create %s: %v", s.email, err)
		}
		userIDByEmail[s.email] = u.ID
		_ = st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{
			AccountUUID:        u.ID,
			PackageName:        s.pkg,
			IncludedQuotaBytes: s.quota,
			PricingRuleVersion: s.rule,
		})
		remaining := s.quota
		if s.email == "buoudidi@outlook.com" {
			remaining = s.quota - (200 << 20)
		}
		_ = st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
			AccountUUID:            u.ID,
			RemainingIncludedQuota: remaining,
		})
	}

	// Include ragged rows where empty trailing `备注` tabs are omitted (10 cells instead of 11)
	// plus CRLF line endings and a trailing blank line.
	tsv := strings.Join([]string{
		"邮箱\t用户名\t订阅开始\t订阅结束\t套餐\t订阅类型\t月配额\t年配额\t费用\t付费状态\t备注",
		"yangzhiyun724@126.com\telsa0724\t2026-08-01\t2027-07-30\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"13191446515@163.com\taudrejing\t2026-07-01\t2027-06-30\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"challen66@163.com\tfuyouhu\t2026-07-01\t2027-06-30\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"sukhadukkha@163.com\tsummer\t2026-05-01\t2027-05-30\tplus\tyear\t30GB\t360GB\t200元\t已付费\t12+1赠送一个月",
		"208746734@qq.com\t\t2026-10-15\t2026-12-15\tplus\tmonth\t30GB\t\t40元\t已付费",
		"304235059@qq.com\t\t2026-09-01\t2027-08-30\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"1324171503@qq.com\t\t2026-10-15\t2026-12-14\tplus\tmonth\t30GB\t\t40元\t已付费",
		"514916831@qq.com\t\t2026-10-07\t2026-12-07\tplus\tmonth\t30GB\t\t20元\t已付费",
		"1767425958@qq.com\t\t2026-10-15\t2027-09-15\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"sccxboy@163.com\t\t2026-09-01\t2027-08-30\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"156405189@qq.com\t\t2026-09-01\t2027-08-30\tpro\tmonth\t30GB\t600GB\t200元\t已付费\t内侧用户",
		"ho_yatphone@163.com\t\t2026-11-02\t2026-12-01\tcustom\tmonth\t1TB\t\t50元\t已付费\tUS 节点专属用户",
		"3667108906@qq.com\t\t2026-11-04\t2026-12-03\tcustom\tmonth\t1TB\t\t80元\t已付费\tUS 节点专属用户",
		"buoudidi@outlook.com\t\t2026-11-27\t2027-11-27\tplus\tyear\t30GB\t360GB\t200元\t已付费",
		"",
	}, "\r\n")

	path := "/api/auth/admin/users/plan-groups/batch"
	w := putAdminPlanGroup(t, router, token, path, gin.H{
		"mode":      "preview",
		"requestId": "roster-14-preview",
		"reason":    "import 14 subscribers",
		"csv":       tsv,
	})
	if w.Code != 200 {
		t.Fatalf("preview 14-row TSV failed: %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		PreviewToken string `json:"previewToken"`
		NoChanges    bool   `json:"noChanges"`
		Changes      []struct {
			UserID string `json:"userId"`
			PlanID string `json:"planId"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.NoChanges || len(preview.Changes) != 14 || preview.PreviewToken == "" {
		t.Fatalf("unexpected 14-row preview: noChanges=%v changes=%d body=%s", preview.NoChanges, len(preview.Changes), w.Body.String())
	}

	applyResp := putAdminPlanGroup(t, router, token, path, gin.H{
		"mode":         "apply",
		"requestId":    "roster-14-preview",
		"reason":       "import 14 subscribers",
		"previewToken": preview.PreviewToken,
	})
	if applyResp.Code != 200 {
		t.Fatalf("apply 14-row TSV failed: %d %s", applyResp.Code, applyResp.Body.String())
	}

	buoudidiID := userIDByEmail["buoudidi@outlook.com"]
	savedUser, err := st.GetUserByID(ctx, buoudidiID)
	if err != nil {
		t.Fatalf("get buoudidi user: %v", err)
	}
	if store.MonthlyQuotaGroup(savedUser) != store.MonthlyPlusQuotaLimitGroup {
		t.Fatalf("expected PLUS quota group for buoudidi, got %v", savedUser.Groups)
	}
	for _, forbidden := range []string{store.OneTimeTrialQuotaGroup} {
		if strings.Contains(strings.Join(savedUser.Groups, "|"), forbidden) {
			t.Fatalf("trial group should be cleared on paid import: %v", savedUser.Groups)
		}
	}
	profile, err := st.GetAccountBillingProfile(ctx, buoudidiID)
	if err != nil || profile.PackageName != "plus" || profile.IncludedQuotaBytes != 30<<30 {
		t.Fatalf("unexpected buoudidi billing profile: %+v %v", profile, err)
	}
	quota, err := st.GetAccountQuotaState(ctx, buoudidiID)
	if err != nil || quota.RemainingIncludedQuota != (30<<30)-(200<<20) {
		t.Fatalf("unexpected buoudidi quota state (usage should be preserved): %+v %v", quota, err)
	}
	record, err := st.GetSubscriptionRecord(ctx, buoudidiID)
	if err != nil || record.PlanCode != "PLUS" || record.SubscriptionType != "year" || record.AnnualQuotaBytes != 360<<30 || record.FeeMinor != 20000 || record.PaymentStatus != "已付费" {
		t.Fatalf("unexpected buoudidi subscription record: %+v %v", record, err)
	}

	// Re-importing the exact same 14-row TSV must detect zero changes.
	noop := putAdminPlanGroup(t, router, token, path, gin.H{
		"mode":      "preview",
		"requestId": "roster-14-noop",
		"reason":    "import 14 subscribers again",
		"csv":       tsv,
	})
	if noop.Code != 200 || !strings.Contains(noop.Body.String(), `"noChanges":true`) {
		t.Fatalf("expected noChanges on re-import: %d %s", noop.Code, noop.Body.String())
	}
}
