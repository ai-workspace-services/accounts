package migrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	schema "account/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// The fixture is created from the native artifact in a new loopback database.
// It never operates on an existing target database or real customer data.
func TestTargetScopeUpgradePostgres(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_API_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL fixture not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "postgres" || cfg.User != "postgres" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		t.Fatal("requires dedicated loopback PostgreSQL postgres fixture")
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	name := "target_scope_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE DATABASE ` + quoteBusiness(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(`DROP DATABASE ` + quoteBusiness(name) + ` WITH (FORCE)`); e != nil {
			t.Errorf("fixture cleanup: %v", e)
		}
	})
	dbCfg := cfg.Copy()
	dbCfg.Database = name
	db := stdlib.OpenDB(*dbCfg)
	db.SetMaxOpenConns(1)
	defer db.Close()
	native, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(native)); err != nil {
		t.Fatal(err)
	}
	missing := []string{"finance_invoices", "finance_operation_events", "finance_operations", "finance_payments", "finance_refunds", "identities", "mfa_recovery_codes", "network_team_members", "network_teams", "referral_campaign_revisions", "referral_codes", "referrals", "referral_rewards", "referral_reward_events", "referral_reward_campaign_snapshots", "referral_payment_risks", "referral_settlements", "referral_settlement_items", "referral_verified_invoices", "subscription_extension_ledger", "team_agent_enrollments"}
	for _, table := range missing {
		if _, err = db.Exec(`DROP TABLE public.` + quoteBusiness(table) + ` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`DROP FUNCTION public.bump_version() CASCADE; DROP FUNCTION public.reject_finance_fact_mutation() CASCADE; DROP FUNCTION public.reject_referral_campaign_mutation() CASCADE; DROP FUNCTION public.snapshot_referral_campaign() CASCADE; DROP FUNCTION public.validate_finance_invoice_subscription() CASCADE;
ALTER TABLE public.users ALTER COLUMN email SET NOT NULL;
ALTER TABLE public.tenant_domains DROP CONSTRAINT tenant_domains_tenant_id_fkey;
ALTER TABLE public.tenant_memberships DROP CONSTRAINT tenant_memberships_tenant_id_fkey;
INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES('00000000-0000-4000-8000-000000000071','compat-fixture','synthetic','compat@example.invalid','00000000-0000-4000-8000-000000000072')`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err = db.QueryRow(`SELECT md5(to_jsonb(u)::text) FROM public.users u WHERE username='compat-fixture'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "sql", "compat", "target-scope-upgrade.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("missing target identity accepted")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = db.Exec(`SELECT set_config('accounts_migration_guard.system_identifier','wrong-target',false),set_config('accounts_migration_guard.database_oid',(SELECT oid::text FROM pg_database WHERE datname=current_database()),false)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("incorrect target identity accepted")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = db.Exec(`SELECT set_config('accounts_migration_guard.system_identifier',(SELECT system_identifier::text FROM pg_control_system()),false),set_config('accounts_migration_guard.database_oid',(SELECT oid::text FROM pg_database WHERE datname=current_database()),false)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE public.identities(id integer)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("partial pre-existing target schema accepted")
	}
	_, _ = db.Exec(`ROLLBACK`)
	var untouched string
	if err = db.QueryRow(`SELECT md5(to_jsonb(u)::text) FROM public.users u WHERE username='compat-fixture'`).Scan(&untouched); err != nil || untouched != before {
		t.Fatal("refused partial schema changed existing data", err)
	}
	if _, err = db.Exec(`DROP TABLE public.identities`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err != nil {
		t.Fatalf("compatibility transaction: %v", err)
	}
	var after string
	if err = db.QueryRow(`SELECT md5(to_jsonb(u)::text) FROM public.users u WHERE username='compat-fixture'`).Scan(&after); err != nil || before != after {
		t.Fatalf("existing row changed: %v", err)
	}
	var checkpoint bool
	if err = db.QueryRow(`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&checkpoint); err != nil || checkpoint {
		t.Fatal("script created an application migration checkpoint", err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range order {
		if err = validateBusinessTargetCatalog(context.Background(), tx, table, tables[table], false); err != nil {
			t.Errorf("%s: %v", table, err)
		}
	}
	_ = tx.Rollback()
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("repeat execution accepted without verifying already-applied state")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if err = db.QueryRow(`SELECT md5(to_jsonb(u)::text) FROM public.users u WHERE username='compat-fixture'`).Scan(&after); err != nil || before != after {
		t.Fatalf("failed retry changed existing row: %v", err)
	}
}
