package store

import (
	"context"
	"testing"
)

func TestCreateUserPreservesSuppliedNetworkCredential(t *testing.T) {
	user := &User{
		Name:      "Canonical User",
		Email:     "canonical@example.com",
		ProxyUUID: "legacy-proxy-uuid",
	}

	if err := NewMemoryStore().CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID == "" {
		t.Fatal("expected account UUID")
	}
	if user.ProxyUUID != "legacy-proxy-uuid" {
		t.Fatalf("expected existing proxy credential to remain unchanged, got %q", user.ProxyUUID)
	}
}

func TestUpdateUserPreservesRotatedNetworkCredential(t *testing.T) {
	st := NewMemoryStore()
	user := &User{Name: "Repair User", Email: "repair@example.com"}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	user.ProxyUUID = "legacy-proxy-uuid"
	if err := st.UpdateUser(context.Background(), user); err != nil {
		t.Fatalf("update user: %v", err)
	}

	stored, err := st.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if stored.ProxyUUID != "legacy-proxy-uuid" {
		t.Fatalf("expected rotated proxy credential to remain unchanged, got %q", stored.ProxyUUID)
	}
}
