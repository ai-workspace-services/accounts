package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestTemporaryUnlimitedPreviewPreservesCommercialFactsAndCanRestore(t *testing.T) {
	router, st, _, target := opsHarness(t)
	ctx := context.Background()
	root := &store.User{Name: "override-root", Email: "override@example.invalid", Role: store.RoleRoot, Active: true, EmailVerified: true}
	if err := st.CreateUser(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "override-root", root.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	seedAdminPlanGroupCatalog(t, st)
	path := "/api/auth/admin/users/plan-groups/batch"
	apply := func(payload gin.H) {
		t.Helper()
		p := putAdminPlanGroup(t, router, "override-root", path, payload)
		if p.Code != 200 {
			t.Fatalf("preview: %s", p.Body.String())
		}
		var preview struct {
			PreviewToken string `json:"previewToken"`
		}
		_ = json.Unmarshal(p.Body.Bytes(), &preview)
		a := putAdminPlanGroup(t, router, "override-root", path, gin.H{"mode": "apply", "reason": payload["reason"], "requestId": payload["requestId"], "previewToken": preview.PreviewToken})
		if a.Code != 200 {
			t.Fatalf("apply: %s", a.Body.String())
		}
	}
	apply(gin.H{"mode": "preview", "requestId": "seed", "reason": "seed", "csv": "email,subscription_type,monthly_quota_bytes,annual_quota_bytes,fee_minor,payment_status,valid_from,valid_until,note\ntarget@example.com,year,21474836480,257698037760,20000,paid,2026-01-01,2030-12-31,gift"})
	user, _ := st.GetUserByID(ctx, target.ID)
	before, _ := st.GetSubscriptionRecord(ctx, target.ID)
	profileBefore, _ := st.GetAccountBillingProfile(ctx, target.ID)
	quotaBefore, _ := st.GetAccountQuotaState(ctx, target.ID)
	if err := st.CreateSession(ctx, "override-target", target.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	groups := append(append([]string{}, user.Groups...), store.SubscriptionUnlimitedOverrideGroup)
	apply(gin.H{"mode": "preview", "requestId": "enable", "reason": "temporary unlimited", "updates": []gin.H{{"userId": target.ID, "groups": groups}}})
	stored, _ := st.GetUserByID(ctx, target.ID)
	if !store.HasSubscriptionUnlimitedOverride(stored, time.Now()) {
		t.Fatal("override not effective")
	}
	usage := opsGet(t, router, "override-target", "/api/account/usage/summary")
	if usage.Code != 200 || !strings.Contains(usage.Body.String(), `"temporaryUnlimited":true`) || !strings.Contains(usage.Body.String(), `"source":"temporary_subscription_override"`) {
		t.Fatalf("effective override missing from usage contract: %s", usage.Body.String())
	}
	after, _ := st.GetSubscriptionRecord(ctx, target.ID)
	profileAfter, _ := st.GetAccountBillingProfile(ctx, target.ID)
	quotaAfter, _ := st.GetAccountQuotaState(ctx, target.ID)
	for _, pair := range [][2]any{{before, after}, {profileBefore, profileAfter}, {quotaBefore, quotaAfter}} {
		b, _ := json.Marshal(pair[0])
		a, _ := json.Marshal(pair[1])
		if string(b) != string(a) {
			t.Fatal("override mutated persisted commercial facts")
		}
	}
	// Empty imported cells preserve existing dates and commercial facts.
	h := &handler{store: st}
	updates, err := h.subscriptionCSVUpdates(ctx, "email,subscription start,subscription end\ntarget@example.com,,", false)
	if err != nil || updates[0].SubscriptionRecord.ValidUntil == nil || updates[0].SubscriptionRecord.FeeMinor != 20000 {
		t.Fatalf("blank reimport erased facts: %v", err)
	}
	_, err = h.subscriptionCSVUpdates(ctx, "email,subscription_type\nmissing@example.invalid,year\ntarget@example.com,bad\nTARGET@example.com,month", false)
	if err == nil || !strings.Contains(err.Error(), "row 2") || !strings.Contains(err.Error(), "row 3") || !strings.Contains(err.Error(), "row 4") {
		t.Fatalf("missing per-row conflict report: %v", err)
	}
	apply(gin.H{"mode": "preview", "requestId": "restore", "reason": "restore original quota", "updates": []gin.H{{"userId": target.ID, "groups": user.Groups}}})
	stored, _ = st.GetUserByID(ctx, target.ID)
	if store.HasSubscriptionUnlimitedOverride(stored, time.Now()) {
		t.Fatal("override could not be revoked")
	}
}
