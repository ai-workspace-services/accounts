package migrate

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestLoadLegacySessionsWithoutUUID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT column_name").WithArgs("sessions").WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("created_at"))
	expires := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT ''::text AS uuid, token, expires_at, user_uuid, created_at FROM sessions WHERE user_uuid IN ($1) ORDER BY created_at ASC")).WithArgs("user-id").WillReturnRows(sqlmock.NewRows([]string{"uuid", "token", "expires_at", "user_uuid", "created_at"}).AddRow("", "synthetic-session-token", expires, "user-id", expires))
	sessions, err := loadSessions(context.Background(), db, []string{"user-id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions count %d", len(sessions))
	}
	if _, err := uuid.Parse(sessions[0].UUID); err != nil {
		t.Fatal("legacy session must receive a stable snapshot UUID")
	}
	if strings.Contains(sessions[0].UUID, "synthetic-session-token") {
		t.Fatal("token leaked into snapshot UUID")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMergeUsesTargetKey(t *testing.T) {
	a := SessionRecord{UUID: "source-id", Token: "synthetic-session-token"}
	b := SessionRecord{UUID: "target-id", Token: a.Token}
	if sessionMergeKey(a, tableColumnCapabilities{}) != sessionMergeKey(b, tableColumnCapabilities{}) {
		t.Fatal("legacy key must match token")
	}
	if sessionMergeKey(a, tableColumnCapabilities{hasUUID: true}) == sessionMergeKey(b, tableColumnCapabilities{hasUUID: true}) {
		t.Fatal("modern UUID identity must remain distinct")
	}
}

func TestUpsertLegacySessionUsesTokenConstraint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`(?s)INSERT INTO sessions \(token, expires_at, user_uuid\).*VALUES \(\$1, \$2, \$3\).*ON CONFLICT \(token\)`).WithArgs("synthetic-session-token", expires, "user-id").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := upsertSession(context.Background(), tx, &SessionRecord{UUID: "unused-source-id", Token: "synthetic-session-token", ExpiresAt: expires, UserUUID: "user-id"}, tableColumnCapabilities{}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
