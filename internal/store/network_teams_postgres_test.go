package store

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"regexp"
	"testing"
)

func TestNetworkMembershipCapIsCheckedUnderTransactionLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_xact_lock(2026101201)`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT member_limit,active FROM network_teams`).WithArgs("acme").WillReturnRows(sqlmock.NewRows([]string{"member_limit", "active"}).AddRow(1, false))
	mock.ExpectQuery(`SELECT COALESCE`).WithArgs("member").WillReturnRows(sqlmock.NewRows([]string{"team_id", "dedicated_only"}).AddRow("", true))
	mock.ExpectQuery(`SELECT count\(\*\) FROM network_team_members`).WithArgs("acme", "member").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()
	if err := st.PutNetworkMembership(context.Background(), NetworkMembership{UserID: "member", TeamID: "acme"}, &AuditLog{}); !errors.Is(err, ErrNetworkTeamFull) {
		t.Fatalf("cap: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkMembershipWriteRollsBackIfAuditFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO network_team_members`).WithArgs("member", "", true).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnError(errors.New("audit unavailable"))
	mock.ExpectRollback()
	if err := st.PutNetworkMembership(context.Background(), NetworkMembership{UserID: "member", DedicatedOnly: true}, &AuditLog{Action: "network.membership.set"}); err == nil {
		t.Fatal("unaudited membership committed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
