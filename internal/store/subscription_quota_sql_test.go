package store

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
	"time"
)

func TestSubscriptionQuotaTransactionRollsBackOnPeriodConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT included_quota_bytes,pricing_rule_version").WithArgs("account").WillReturnRows(sqlmock.NewRows([]string{"included", "rule"}).AddRow(20, "plan:PLUS"))
	mock.ExpectExec("UPDATE public.account_quota_states").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	now := time.Now().UTC()
	err = (&postgresStore{db: db}).UpgradeSubscriptionQuota(context.Background(), "account", 20, 50, now, now.AddDate(0, 1, 0))
	if !errors.Is(err, ErrSubscriptionQuotaConflict) {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionQuotaReplayDoesNotWriteOrRefill(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT included_quota_bytes,pricing_rule_version").WillReturnRows(sqlmock.NewRows([]string{"included", "rule"}).AddRow(50, "plan:PRO-MONTHLY-V2"))
	mock.ExpectCommit()
	now := time.Now().UTC()
	if err = (&postgresStore{db: db}).UpgradeSubscriptionQuota(context.Background(), "account", 20, 50, now, now.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
