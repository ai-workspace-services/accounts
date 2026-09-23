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

	if err := st.DeleteUser(ctx, user.ID); err != nil {
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
	if err := st.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("repeated archive should be idempotent: %v", err)
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
	if err := NewMemoryStore().DeleteUser(context.Background(), "missing"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestCanArchiveUserRequiresBothSafetyColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps schemaCapabilities
		want bool
	}{
		{name: "both columns", caps: schemaCapabilities{hasArchivedAt: true, hasActive: true}, want: true},
		{name: "archive only", caps: schemaCapabilities{hasArchivedAt: true}, want: false},
		{name: "active only", caps: schemaCapabilities{hasActive: true}, want: false},
		{name: "neither", caps: schemaCapabilities{}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canArchiveUser(tc.caps); got != tc.want {
				t.Fatalf("canArchiveUser(%+v) = %v, want %v", tc.caps, got, tc.want)
			}
		})
	}
}
