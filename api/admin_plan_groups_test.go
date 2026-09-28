package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/store"
)

const adminPlanTestGiB = int64(1024 * 1024 * 1024)

func putAdminPlanGroup(t *testing.T, router http.Handler, token, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func seedAdminPlanGroupCatalog(t *testing.T, st store.Store) {
	t.Helper()
	plans := []store.BillingPlan{
		{PlanID: store.BillingPlanFree, DisplayName: "Free", Kind: "subscription", PackageName: "free", IncludedQuotaBytes: 5 * adminPlanTestGiB, Active: true},
		{PlanID: store.BillingPlanPlus, DisplayName: "Plus", Kind: "subscription", PackageName: "plus", IncludedQuotaBytes: 20 * adminPlanTestGiB, Active: true},
		{PlanID: store.BillingPlanUnlimitedBeta, DisplayName: "Unlimited Beta", Kind: "subscription", PackageName: "unlimited-beta", IncludedQuotaBytes: 0, Active: true},
	}
	for i := range plans {
		if err := st.UpsertBillingPlan(context.Background(), &plans[i]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdminPlanGroupPreviewApplyUsesOneTimeTokenAndSynchronizesQuota(t *testing.T) {
	router, st, token, target := opsHarness(t)
	seedAdminPlanGroupCatalog(t, st)
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: target.ID, PackageName: "free", IncludedQuotaBytes: 5 * adminPlanTestGiB, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: target.ID, RemainingIncludedQuota: 0, PeriodStart: &start, PeriodEnd: &end, EffectiveAt: start}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrafficMinuteBucket(ctx, &store.TrafficMinuteBucket{AccountUUID: target.ID, BucketStart: start.Add(time.Hour), TotalBytes: 6 * adminPlanTestGiB}); err != nil {
		t.Fatal(err)
	}

	path := "/api/auth/admin/users/" + target.ID + "/plan-group"
	previewPayload := map[string]any{"mode": "preview", "requestId": "plan-change-42", "reason": "support ticket #42", "updates": []map[string]any{{
		"planId": store.BillingPlanPlus, "validFrom": "2026-09-01", "validUntil": "2026-10-31",
	}}}
	previewRec := putAdminPlanGroup(t, router, token, path, previewPayload)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", previewRec.Code, previewRec.Body.String())
	}
	var preview struct {
		PreviewToken string `json:"previewToken"`
		Changes      []struct {
			Used      int64 `json:"usedBytesPreserved"`
			Remaining int64 `json:"remainingIncludedQuota"`
			Paused    bool  `json:"configurationSyncWillPause"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.PreviewToken == "" || len(preview.Changes) != 1 || preview.Changes[0].Used != 6*adminPlanTestGiB || preview.Changes[0].Remaining != 14*adminPlanTestGiB || preview.Changes[0].Paused {
		t.Fatalf("preview did not describe usage-preserving quota recalculation: %+v", preview)
	}

	applyPayload := map[string]any{"mode": "apply", "requestId": "plan-change-42", "reason": "support ticket #42", "previewToken": preview.PreviewToken}
	forged := preview.PreviewToken[:len(preview.PreviewToken)-1] + "0"
	if forged == preview.PreviewToken {
		forged = preview.PreviewToken[:len(preview.PreviewToken)-1] + "1"
	}
	applyPayload["previewToken"] = forged
	if rec := putAdminPlanGroup(t, router, token, path, applyPayload); rec.Code != http.StatusConflict {
		t.Fatalf("forged preview token must be rejected, got %d: %s", rec.Code, rec.Body.String())
	}
	if profile, _ := st.GetAccountBillingProfile(ctx, target.ID); profile.PackageName != "free" {
		t.Fatalf("forged token changed billing profile: %+v", profile)
	}

	applyPayload["previewToken"] = preview.PreviewToken
	applyRec := putAdminPlanGroup(t, router, token, path, applyPayload)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", applyRec.Code, applyRec.Body.String())
	}
	user, err := st.GetUserByID(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if store.MonthlyQuotaGroup(user) != store.MonthlyPlusQuotaLimitGroup || user.SubscriptionValidUntil == nil || user.SubscriptionValidUntil.Format("2006-01-02") != "2026-10-31" {
		t.Fatalf("user plan group or validity not updated: %+v", user)
	}
	profile, err := st.GetAccountBillingProfile(ctx, target.ID)
	if err != nil || profile.PackageName != "plus" || profile.IncludedQuotaBytes != 20*adminPlanTestGiB {
		t.Fatalf("billing profile not synchronized: %+v err=%v", profile, err)
	}
	quota, err := st.GetAccountQuotaState(ctx, target.ID)
	if err != nil || quota.RemainingIncludedQuota != 14*adminPlanTestGiB || quota.PeriodStart == nil || !quota.PeriodStart.Equal(start) {
		t.Fatalf("quota state not synchronized or period reset: %+v err=%v", quota, err)
	}

	downPreview := putAdminPlanGroup(t, router, token, path, map[string]any{"mode": "preview", "requestId": "plan-downgrade-1", "reason": "downgrade test", "updates": []map[string]any{{"planId": store.BillingPlanFree}}})
	if downPreview.Code != http.StatusOK {
		t.Fatalf("downgrade preview: %d %s", downPreview.Code, downPreview.Body.String())
	}
	var downBody struct {
		PreviewToken string `json:"previewToken"`
		Changes      []struct {
			Remaining int64 `json:"remainingIncludedQuota"`
			Paused    bool  `json:"configurationSyncWillPause"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(downPreview.Body.Bytes(), &downBody); err != nil {
		t.Fatal(err)
	}
	if len(downBody.Changes) != 1 || downBody.Changes[0].Remaining != 0 || !downBody.Changes[0].Paused {
		t.Fatalf("over-cap downgrade preview must show immediate config-sync pause: %+v", downBody)
	}
	if rec := putAdminPlanGroup(t, router, token, path, map[string]any{"mode": "apply", "requestId": "plan-downgrade-1", "reason": "downgrade test", "previewToken": downBody.PreviewToken}); rec.Code != http.StatusOK {
		t.Fatalf("downgrade apply: %d %s", rec.Code, rec.Body.String())
	}
	upPreview := putAdminPlanGroup(t, router, token, path, map[string]any{"mode": "preview", "requestId": "plan-upgrade-2", "reason": "upgrade test", "updates": []map[string]any{{"planId": store.BillingPlanPlus}}})
	if upPreview.Code != http.StatusOK {
		t.Fatalf("upgrade preview: %d %s", upPreview.Code, upPreview.Body.String())
	}
	var upBody struct {
		PreviewToken string `json:"previewToken"`
		Changes      []struct {
			Remaining int64 `json:"remainingIncludedQuota"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(upPreview.Body.Bytes(), &upBody); err != nil {
		t.Fatal(err)
	}
	if len(upBody.Changes) != 1 || upBody.Changes[0].Remaining != 14*adminPlanTestGiB {
		t.Fatalf("upgrade preview reset usage after downgrade: %+v", upBody)
	}
	if rec := putAdminPlanGroup(t, router, token, path, map[string]any{"mode": "apply", "requestId": "plan-upgrade-2", "reason": "upgrade test", "previewToken": upBody.PreviewToken}); rec.Code != http.StatusOK {
		t.Fatalf("upgrade apply: %d %s", rec.Code, rec.Body.String())
	}

	replay := putAdminPlanGroup(t, router, token, path, applyPayload)
	if replay.Code != http.StatusOK {
		t.Fatalf("idempotent replay: %d %s", replay.Code, replay.Body.String())
	}
	var replayBody struct {
		Replayed bool `json:"replayed"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayBody); err != nil || !replayBody.Replayed {
		t.Fatalf("expected idempotent replay, payload=%s err=%v", replay.Body.String(), err)
	}
	audit, err := st.ListAuditLogs(ctx, store.AuditLogFilter{ActionPrefix: store.AuditActionPlanGroupUpdate})
	if err != nil || len(audit) != 3 {
		t.Fatalf("expected three audited applications, count=%d err=%v", len(audit), err)
	}
}
