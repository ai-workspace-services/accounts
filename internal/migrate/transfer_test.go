package migrate

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	accountschema "account/sql"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestRejectUserUUIDRekeysBeforeWrites(t *testing.T) {
	err := rejectUserUUIDRekeys([]userUUIDRekey{{
		Source: "source-user",
		Target: "target-user",
	}})
	if err == nil {
		t.Fatal("expected rekey to be rejected")
	}
	for _, want := range []string{"source-user", "target-user", "before writes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if err := rejectUserUUIDRekeys(nil); err != nil {
		t.Fatalf("empty rekey plan should be allowed: %v", err)
	}
}

func TestProtectedImportOptionsRequireMergeBeforeOpeningDatabase(t *testing.T) {
	for _, opts := range []ImportOptions{
		{PreserveExistingUsers: true},
		{SkipSessions: true},
	} {
		_, err := NewImporter().Import(context.Background(), "postgres://unreachable", &AccountDump{
			Metadata: &SnapshotMetadata{Version: SnapshotVersion, SchemaHash: accountschema.Hash()},
		}, opts)
		if err == nil || !strings.Contains(err.Error(), "require merge mode") {
			t.Fatalf("expected merge guard for %+v, got %v", opts, err)
		}
	}
}

func TestValidateMergeUserConflictsRejectsExistingUsernameBeforeWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT uuid::text FROM users WHERE lower(username) = lower($1) AND uuid <> $2 LIMIT 1`)).
		WithArgs("SharedName", "incoming-user").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow("existing-user"))

	err = validateMergeUserConflicts(context.Background(), db, []UserRecord{{
		UUID:     "incoming-user",
		Username: "SharedName",
		Email:    "new@example.test",
		Role:     "user",
	}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "belongs to existing user existing-user") {
		t.Fatalf("expected explicit username conflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMergeUserConflictsRejectsExistingEmailBeforeWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT uuid::text FROM users WHERE lower(username) = lower($1) AND uuid <> $2 LIMIT 1`)).
		WithArgs("IncomingName", "incoming-user").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT uuid::text FROM users WHERE lower(email) = lower($1) AND uuid <> $2 LIMIT 1`)).
		WithArgs("shared@example.test", "incoming-user").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow("existing-user"))

	err = validateMergeUserConflicts(context.Background(), db, []UserRecord{{
		UUID:     "incoming-user",
		Username: "IncomingName",
		Email:    "shared@example.test",
		Role:     "user",
	}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "belongs to existing user existing-user") {
		t.Fatalf("expected explicit email conflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertUserMergeDoesNotDeleteConflictingUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO users \(`).WillReturnError(errors.New("duplicate key value violates unique constraint"))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	err = upsertUser(context.Background(), tx, &UserRecord{
		UUID:         "incoming-user",
		ProxyUUID:    "proxy-user",
		Username:     "shared-name",
		PasswordHash: "hash",
		Email:        "shared@example.test",
		Role:         "user",
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("expected unique constraint error, got %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback failed merge: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
