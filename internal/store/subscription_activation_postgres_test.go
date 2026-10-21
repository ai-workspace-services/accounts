package store

import (
	schema "account/sql"
	"context"
	"database/sql"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

func TestSubscriptionActivationPostgresTransaction(t *testing.T) {
	dsn := os.Getenv("ACTIVATION_QUOTA_TEST_DSN")
	if dsn == "" {
		t.Skip("requires fresh local activation_quota_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || cfg.Database != "activation_quota_test" {
		t.Fatal("only the disposable local activation_quota_test database is accepted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var exists bool
	if err := db.QueryRow("SELECT to_regclass('public.users') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("requires an empty database: exists=%v err=%v", exists, err)
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
	u := &User{Name: "Activation", Email: "activation@example.invalid", Active: true, Role: RoleUser}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: u.ID, PackageName: "free", IncludedQuotaBytes: 5 << 30, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	month := time.Now().UTC()
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 0, CurrentBalance: 12, ProxyAccessState: "paused", SuspendState: "active", PeriodStart: &start, PeriodEnd: &end}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE FUNCTION reject_activation_profile() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected profile failure'; END $$; CREATE TRIGGER reject_activation_profile BEFORE UPDATE ON account_billing_profiles FOR EACH ROW EXECUTE FUNCTION reject_activation_profile()`); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateSubscriptionQuota(ctx, u.ID, 5<<30, 20<<30, start, end); err == nil {
		t.Fatal("expected transaction failure")
	}
	q, _ := st.GetAccountQuotaState(ctx, u.ID)
	p, _ := st.GetAccountBillingProfile(ctx, u.ID)
	if q.RemainingIncludedQuota != 0 || p.IncludedQuotaBytes != 5<<30 {
		t.Fatal("failed activation partially committed")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_activation_profile ON account_billing_profiles; DROP FUNCTION reject_activation_profile()`); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateSubscriptionQuota(ctx, u.ID, 5<<30, 20<<30, start, end); err != nil {
		t.Fatal(err)
	}
	q, _ = st.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 20<<30 || q.CurrentBalance != 12 || q.ProxyAccessState != "paused" {
		t.Fatalf("invalid activation or operator pause overwritten: %+v", q)
	}
	q.RemainingIncludedQuota -= 3 << 30
	if err := st.UpsertAccountQuotaState(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateSubscriptionQuota(ctx, u.ID, 5<<30, 20<<30, start, end); err != nil {
		t.Fatal(err)
	}
	q, _ = st.GetAccountQuotaState(ctx, u.ID)
	if q.RemainingIncludedQuota != 17<<30 {
		t.Fatalf("replay replenished consumed quota: %+v", q)
	}
}
