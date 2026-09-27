package migrate

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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

func TestInsertImportedUserDoesNotDeleteOrOverwriteConflictingUsers(t *testing.T) {
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
	err = insertImportedUser(context.Background(), tx, &UserRecord{
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

func TestPrepareImportedUserPreservesExistingProfileForEveryMergeStrategy(t *testing.T) {
	existing := UserRecord{
		UUID:         "existing-user",
		ProxyUUID:    "existing-proxy",
		Username:     "local-name",
		PasswordHash: "local-password-hash",
		Email:        "local@example.test",
		Level:        17,
		Role:         "user",
		Groups:       []string{"local-group"},
		Permissions:  []string{"local-permission"},
	}
	incoming := UserRecord{
		UUID:         "existing-user",
		ProxyUUID:    "incoming-proxy",
		Username:     "snapshot-name",
		PasswordHash: "snapshot-password-hash",
		Email:        "snapshot@example.test",
		Level:        99,
		Role:         "admin",
		Groups:       []string{"snapshot-group"},
		Permissions:  []string{"snapshot-permission"},
	}

	for _, strategy := range []MergeStrategy{MergeStrategyAppend, MergeStrategyTimestamp, MergeStrategyReplace} {
		t.Run(string(strategy), func(t *testing.T) {
			got, changed := prepareImportedUser(incoming, existing, ImportOptions{
				Merge:         true,
				MergeStrategy: strategy,
			}, true)
			if changed {
				t.Fatal("existing profile must not be marked for update")
			}
			if !reflect.DeepEqual(got, existing) {
				t.Fatalf("existing profile changed during import: got=%+v want=%+v", got, existing)
			}
		})
	}
}

func TestRequireEmptyUsersForReplace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasUsers bool
		wantErr  bool
	}{
		{name: "empty target"},
		{name: "existing users", hasUsers: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("new sql mock: %v", err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta(`LOCK TABLE public.users IN SHARE ROW EXCLUSIVE MODE`)).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS (SELECT 1 FROM public.users)`)).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.hasUsers))
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			err = requireEmptyUsersForReplace(context.Background(), tx)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireEmptyUsersForReplace() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				mock.ExpectRollback()
				if rollbackErr := tx.Rollback(); rollbackErr != nil {
					t.Fatalf("rollback transaction: %v", rollbackErr)
				}
			} else {
				mock.ExpectCommit()
				if commitErr := tx.Commit(); commitErr != nil {
					t.Fatalf("commit transaction: %v", commitErr)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
