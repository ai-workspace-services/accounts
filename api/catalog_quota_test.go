package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/store"
)

func TestAdminPlanSavePropagatesQuotaWithoutResettingUsage(t *testing.T) {
	router, st, token, target := opsHarness(t)
	ctx := context.Background()
	target.Groups = []string{store.MonthlyFreeQuotaLimitGroup}
	if err := st.UpdateUser(ctx, target); err != nil {
		t.Fatal(err)
	}
	start, end := naturalMonthPeriod(time.Now().UTC())
	const gib = int64(1 << 30)
	if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: target.ID, IncludedQuotaBytes: 3 * gib, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: target.ID, RemainingIncludedQuota: 2 * gib, PeriodStart: &start, PeriodEnd: &end}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/billing/plans/FREE", strings.NewReader(`{"displayName":"Free","kind":"subscription","packageName":"free","includedQuotaBytes":5368709120,"active":true,"reason":"raise free quota to 5 GiB"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	p, err := st.GetAccountBillingProfile(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	q, err := st.GetAccountQuotaState(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.IncludedQuotaBytes != 5*gib || q.RemainingIncludedQuota != 4*gib {
		t.Fatalf("quota not propagated: %+v %+v", p, q)
	}
	if !q.PeriodStart.Equal(start) || !q.PeriodEnd.Equal(end) {
		t.Fatal("billing period reset")
	}
	if err := st.CreateSession(ctx, "quota-reader", target.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	read := httptest.NewRequest(http.MethodGet, "/api/account/usage/summary", nil)
	read.Header.Set("Authorization", "Bearer quota-reader")
	result := httptest.NewRecorder()
	router.ServeHTTP(result, read)
	if result.Code != http.StatusOK {
		t.Fatalf("user quota read: %d %s", result.Code, result.Body.String())
	}
	var summary struct {
		IncludedQuotaBytes     int64 `json:"includedQuotaBytes"`
		RemainingIncludedQuota int64 `json:"remainingIncludedQuota"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.IncludedQuotaBytes != 5*gib || summary.RemainingIncludedQuota != 4*gib {
		t.Fatalf("user still sees stale quota: %s", result.Body.String())
	}
}
