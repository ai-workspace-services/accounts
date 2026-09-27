package store

import (
	"context"
	"testing"
	"time"
)

func TestMFARecoveryCodesRotateExpireConsumeAndRevokeSessions(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{Name: "Protected", Email: "protected@example.invalid", Role: RoleAdmin, Groups: []string{MonthlyPlusQuotaLimitGroup}, Active: true}
	validUntil := time.Now().UTC().Add(30 * 24 * time.Hour)
	user.SubscriptionValidUntil = &validUntil
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.UpsertSubscription(ctx, &Subscription{ID: "subscription-1", UserID: user.ID, ExternalID: "stripe-subscription-1", Provider: "stripe", PlanID: "plus", Status: "active"}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	for _, token := range []string{"current-session", "other-device-session"} {
		if err := st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	now := time.Now().UTC()
	oldBatch := []MFARecoveryCode{{ID: "old-code", UserID: user.ID, BatchID: "old-batch", CodeHash: "hash-old", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	if err := st.ReplaceMFARecoveryCodes(ctx, user.ID, oldBatch); err != nil {
		t.Fatalf("store first batch: %v", err)
	}
	newBatch := []MFARecoveryCode{
		{ID: "new-code-1", UserID: user.ID, BatchID: "new-batch", CodeHash: "hash-new-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "new-code-2", UserID: user.ID, BatchID: "new-batch", CodeHash: "hash-new-2", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	if err := st.ReplaceMFARecoveryCodes(ctx, user.ID, newBatch); err != nil {
		t.Fatalf("rotate batch: %v", err)
	}
	active, err := st.ListMFARecoveryCodes(ctx, user.ID, now)
	if err != nil || len(active) != 2 {
		t.Fatalf("expected the new active batch, got %d codes and error %v", len(active), err)
	}
	for _, code := range active {
		if code.ID == "old-code" || code.CodeHash == "" {
			t.Fatalf("old batch was not revoked or hash missing: %#v", code)
		}
	}

	if err := st.CompleteMFAPasswordReset(ctx, user.ID, "new-code-1", "new-password-hash", now); err != nil {
		t.Fatalf("complete recovery reset: %v", err)
	}
	if _, _, err := st.GetSession(ctx, "current-session"); err == nil {
		t.Fatal("current session was not revoked")
	}
	if _, _, err := st.GetSession(ctx, "other-device-session"); err == nil {
		t.Fatal("other-device session was not revoked")
	}
	if err := st.CompleteMFAPasswordReset(ctx, user.ID, "new-code-1", "replay-hash", now); err != ErrMFARecoveryCodeInvalid {
		t.Fatalf("expected consumed code replay rejection, got %v", err)
	}

	reloaded, err := st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if reloaded.PasswordHash != "new-password-hash" || reloaded.Role != RoleAdmin || MonthlyQuotaGroup(reloaded) != MonthlyPlusQuotaLimitGroup || reloaded.SubscriptionValidUntil == nil || !reloaded.SubscriptionValidUntil.Equal(validUntil) {
		t.Fatalf("password reset changed protected account attributes: %#v", reloaded)
	}
	subscriptions, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil || len(subscriptions) != 1 || subscriptions[0].Status != "active" {
		t.Fatalf("subscription was changed or removed: %#v, %v", subscriptions, err)
	}

	if err := st.RevokeMFARecoveryCodes(ctx, user.ID, now); err != nil {
		t.Fatalf("revoke remaining recovery codes: %v", err)
	}
	active, err = st.ListMFARecoveryCodes(ctx, user.ID, now)
	if err != nil || len(active) != 0 {
		t.Fatalf("expected remaining codes to be revoked, got %d and %v", len(active), err)
	}
}

func TestMFARecoveryCodeAttemptLimitAndExpiry(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{Name: "Code User", Email: "codes@example.invalid", Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	if err := st.ReplaceMFARecoveryCodes(ctx, user.ID, []MFARecoveryCode{{
		ID: "limited-code", UserID: user.ID, BatchID: "batch", CodeHash: "hash", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}); err != nil {
		t.Fatalf("store code: %v", err)
	}
	lockedUntil, err := st.RecordMFARecoveryCodeFailure(ctx, user.ID, now, 2, 15*time.Minute)
	if err != nil || !lockedUntil.IsZero() {
		t.Fatalf("first failure should not lock the set: %v %v", lockedUntil, err)
	}
	lockedUntil, err = st.RecordMFARecoveryCodeFailure(ctx, user.ID, now.Add(time.Second), 2, 15*time.Minute)
	if err != nil || !lockedUntil.After(now) {
		t.Fatalf("second failure should lock the set: %v %v", lockedUntil, err)
	}
	if _, err := st.ListMFARecoveryCodes(ctx, user.ID, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("expired-code lookup should be safe: %v", err)
	} else if codes, _ := st.ListMFARecoveryCodes(ctx, user.ID, now.Add(2*time.Hour)); len(codes) != 0 {
		t.Fatalf("expired codes remained usable: %#v", codes)
	}
}

func TestArchivedAccountCannotCompleteMFARecoveryReset(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	archivedAt := time.Now().UTC()
	user := &User{Name: "Archived", Email: "archived@example.invalid", Active: true, ArchivedAt: &archivedAt, PasswordHash: "old-hash"}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	if err := st.ReplaceMFARecoveryCodes(ctx, user.ID, []MFARecoveryCode{{
		ID: "archived-code", UserID: user.ID, BatchID: "batch", CodeHash: "hash", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}); err != nil {
		t.Fatalf("store code: %v", err)
	}
	if err := st.CompleteMFAPasswordReset(ctx, user.ID, "archived-code", "new-hash", now); err != ErrMFARecoveryCodeInvalid {
		t.Fatalf("expected archived account reset rejection, got %v", err)
	}
	reloaded, err := st.GetUserByID(ctx, user.ID)
	if err != nil || reloaded.PasswordHash != "old-hash" {
		t.Fatalf("archived account password changed: %#v %v", reloaded, err)
	}
}
