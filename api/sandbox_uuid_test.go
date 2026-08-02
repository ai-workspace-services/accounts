package api

import (
	"context"
	"testing"
	"time"

	"account/internal/store"
)

func TestEnsureSandboxProxyUUIDRenewsExpiryWithoutChangingIdentity(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	sandbox := &store.User{
		Name:          "Sandbox",
		Email:         sandboxUserEmail,
		EmailVerified: true,
		Active:        true,
	}
	if err := st.CreateUser(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox user: %v", err)
	}

	originalID := sandbox.ID
	sandbox.ProxyUUID = "legacy-proxy-uuid"
	expired := time.Now().UTC().Add(-time.Minute)
	sandbox.ProxyUUIDExpiresAt = &expired
	if err := st.UpdateUser(ctx, sandbox); err != nil {
		t.Fatalf("seed expired sandbox access: %v", err)
	}

	h := &handler{store: st}
	if err := h.ensureSandboxProxyUUID(ctx, sandbox); err != nil {
		t.Fatalf("renew sandbox access: %v", err)
	}

	updated, err := st.GetUserByID(ctx, originalID)
	if err != nil {
		t.Fatalf("reload sandbox user: %v", err)
	}
	if updated.ProxyUUID != originalID {
		t.Fatalf("expected sandbox proxy UUID %q, got %q", originalID, updated.ProxyUUID)
	}
	if updated.ProxyUUIDExpiresAt == nil || !updated.ProxyUUIDExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expected sandbox access expiry to be renewed, got %v", updated.ProxyUUIDExpiresAt)
	}
}
