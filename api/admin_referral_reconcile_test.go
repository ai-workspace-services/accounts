package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"account/internal/store"
)

func TestAdminReferralReconciliationPreviewAndExplicitApply(t *testing.T) {
	f := newReferralCompletionFixture(t, false, true)
	f.router.POST("/reconcile", f.h.adminReconcileReferralInvoices)
	ctx := context.Background()
	admin := &store.User{ID: "reconcile-operator", Name: "Reconcile operator", Email: "reconcile-operator@example.test", Active: true, Role: store.RoleAdmin, Permissions: []string{permissionAdminBillingMoneyWrite}}
	if err := f.h.store.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	for token, id := range map[string]string{"operator-session": admin.ID, "member-session": f.invitee.ID} {
		if err := f.h.store.CreateSession(ctx, token, id, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(fmt.Sprintf(`{"accountUuid":%q,"invoiceIds":["in-completion-first"]}`, f.invitee.ID))
	for _, token := range []string{"", "member-session"} {
		rec := referralContractRequest(t, f.router, http.MethodPost, "/reconcile", token, body)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("unauthorized reconciliation status=%d", rec.Code)
		}
	}
	if rec := referralContractRequest(t, f.router, http.MethodPost, "/reconcile", "operator-session", body); rec.Code != http.StatusOK {
		t.Fatalf("preview failed: %d", rec.Code)
	}
	if rewards, err := f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10); err != nil || len(rewards) != 0 {
		t.Fatal("preview wrote reward facts")
	}
	missingReason := []byte(fmt.Sprintf(`{"accountUuid":%q,"invoiceIds":["in-completion-first"],"apply":true}`, f.invitee.ID))
	if rec := referralContractRequest(t, f.router, http.MethodPost, "/reconcile", "operator-session", missingReason); rec.Code != http.StatusBadRequest {
		t.Fatal("apply without audit reason was accepted")
	}
	apply := []byte(fmt.Sprintf(`{"accountUuid":%q,"invoiceIds":["in-completion-first"],"apply":true,"reason":"reviewed missed first payment"}`, f.invitee.ID))
	for i := 0; i < 2; i++ {
		if rec := referralContractRequest(t, f.router, http.MethodPost, "/reconcile", "operator-session", apply); rec.Code != http.StatusOK {
			t.Fatalf("explicit apply failed: %d", rec.Code)
		}
	}
	if rewards, err := f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10); err != nil || len(rewards) != 1 || rewards[0].RewardMinor != 1234 {
		t.Fatal("reconciliation did not retain exactly one correct reward")
	}
}
