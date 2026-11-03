package store

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
	"time"
)

func TestMonthlyResetAuditFailureRollsBack(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m.ExpectBegin()
	m.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectQuery("SELECT p.included_quota_bytes").WillReturnRows(sqlmock.NewRows([]string{"included", "remaining", "email"}).AddRow(100, 4, "target@example.invalid"))
	m.ExpectExec("UPDATE account_quota_states").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec("INSERT INTO audit_logs").WillReturnError(errors.New("audit unavailable"))
	m.ExpectRollback()
	_, err = (&postgresStore{db: db}).ResetMonthlyUsage(context.Background(), "target", "actor", "target@example.invalid", "test", time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("audit failure ignored")
	}
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
