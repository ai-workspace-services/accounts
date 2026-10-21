package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	schema "account/sql"
	"github.com/jackc/pgx/v5"
)

// The numeric comparison in CreditTopUp must prepare on PostgreSQL, not just
// pass SQL mocks. Exercise rollback and replay against an isolated native DB.
func TestCreditTopUpPostgresRollbackAndReplay(t *testing.T) {
	dsn := os.Getenv("PAYG_TOPUP_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable local payg_topup_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || cfg.Database != "payg_topup_test" {
		t.Fatal("only local payg_topup_test is accepted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var exists bool
	if err := db.QueryRow("SELECT to_regclass('public.users') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("requires empty database", err)
	}
	artifact, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(artifact)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("SET search_path TO public"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st := &postgresStore{db: db}
	user := &User{Name: "Topup", Email: "topup@example.invalid", Active: true, Role: RoleUser}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	state := &AccountQuotaState{AccountUUID: user.ID, CurrentBalance: -5.5, RemainingIncludedQuota: 0, Arrears: true, SuspendState: "active", ProxyAccessState: "paused"}
	if err := st.UpsertAccountQuotaState(ctx, state); err != nil {
		t.Fatal(err)
	}
	entry := &BillingLedgerEntry{ID: "00000000-0000-4000-8000-000000000321", AccountUUID: user.ID, BucketStart: time.Now().UTC(), BucketEnd: time.Now().UTC(), Currency: "CNY", AmountDelta: 200, PricingRuleVersion: "stripe:pi_topup_test"}
	if _, err := db.Exec(`CREATE FUNCTION reject_topup_balance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected topup failure'; END $$; CREATE TRIGGER reject_topup_balance BEFORE UPDATE ON account_quota_states FOR EACH ROW EXECUTE FUNCTION reject_topup_balance()`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreditTopUp(ctx, entry); err == nil {
		t.Fatal("expected update failure")
	}
	ledger, err := st.ListBillingLedgerByAccount(ctx, user.ID, 20)
	if err != nil || len(ledger) != 0 {
		t.Fatal("failed transaction leaked ledger", err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_topup_balance ON account_quota_states; DROP FUNCTION reject_topup_balance()`); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		duplicate, err := st.CreditTopUp(ctx, entry)
		if err != nil || duplicate != (attempt > 0) {
			t.Fatalf("attempt %d duplicate=%v err=%v", attempt, duplicate, err)
		}
	}
	state, err = st.GetAccountQuotaState(ctx, user.ID)
	if err != nil || state.CurrentBalance != 194.5 || state.Arrears || state.ProxyAccessState != "paused" || state.RemainingIncludedQuota != 0 {
		t.Fatalf("bad recovered balance or operator pause changed: %+v err=%v", state, err)
	}
	ledger, err = st.ListBillingLedgerByAccount(ctx, user.ID, 20)
	if err != nil || len(ledger) != 1 || ledger[0].AmountDelta != 200 || ledger[0].BalanceAfter != 194.5 {
		t.Fatalf("ledger mismatch %+v err=%v", ledger, err)
	}
}
