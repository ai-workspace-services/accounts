package store

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryDeleteUserArchivesWithoutRemovingIdentity(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{
		ID: "user-archive-1", Name: "archive target", Email: "archive@example.com",
		Role: RoleUser, Active: true,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	audit := userArchiveTestAudit(user.ID)
	if err := st.DeleteUser(ctx, user.ID, audit, "request-archive-1"); err != nil {
		t.Fatalf("archive user: %v", err)
	}
	archived, err := st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("archived user should remain addressable: %v", err)
	}
	if archived.Active || archived.ArchivedAt == nil {
		t.Fatalf("expected inactive archived user, got active=%v archivedAt=%v", archived.Active, archived.ArchivedAt)
	}
	if _, err := st.GetUserByEmail(ctx, user.Email); err != nil {
		t.Fatalf("archiving must preserve email lookup: %v", err)
	}
	if _, err := st.GetUserByName(ctx, user.Name); err != nil {
		t.Fatalf("archiving must preserve name lookup: %v", err)
	}

	firstArchivedAt := archived.ArchivedAt
	if err := st.DeleteUser(ctx, user.ID, userArchiveTestAudit(user.ID), "request-archive-2"); !errors.Is(err, ErrUserAlreadyArchived) {
		t.Fatalf("repeated archive should be refused, got %v", err)
	}
	archived, err = st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload archived user: %v", err)
	}
	if !archived.ArchivedAt.Equal(*firstArchivedAt) {
		t.Fatalf("repeated archive must preserve original timestamp: first=%v next=%v", firstArchivedAt, archived.ArchivedAt)
	}

	replacement := &User{ID: "user-archive-2", Name: "replacement", Email: user.Email, Active: true}
	if err := st.CreateUser(ctx, replacement); !errors.Is(err, ErrEmailExists) {
		t.Fatalf("archived identity must remain reserved, expected ErrEmailExists, got %v", err)
	}
}

func TestMemoryDeleteUserUnknownID(t *testing.T) {
	if err := NewMemoryStore().DeleteUser(context.Background(), "missing", userArchiveTestAudit("missing"), ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestMemoryDeleteUserRejectsMissingAuditWithoutMutation(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{ID: "user-audit-required", Name: "audit target", Email: "audit@example.com", Role: RoleUser}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.DeleteUser(ctx, user.ID, nil, ""); err == nil {
		t.Fatal("expected missing audit to be rejected")
	}
	stored, err := st.GetUserByID(ctx, user.ID)
	if err != nil || !stored.Active || stored.ArchivedAt != nil {
		t.Fatalf("invalid audit must leave user unchanged: user=%+v err=%v", stored, err)
	}
	entries, err := st.ListAuditLogs(ctx, AuditLogFilter{ActionPrefix: AuditActionUserArchive})
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid audit must not append audit log: entries=%v err=%v", entries, err)
	}
}

func userArchiveTestAudit(userID string) *AuditLog {
	return &AuditLog{
		Action: AuditActionUserArchive, ActorUUID: "operator-test",
		Details: map[string]any{"target_uuid": userID, "reason": "test archive"},
	}
}
