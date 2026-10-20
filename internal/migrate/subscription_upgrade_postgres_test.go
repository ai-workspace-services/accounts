package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

// A disposable, minimal old-generation contract proves that the actual checked-in
// migration is additive and atomic. Full V3 snapshot recovery is a separate gate.
func TestSubscriptionLifecycleUpgradePostgres17(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_LIFECYCLE_UPGRADE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated lifecycle_upgrade_test fixture not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || !strings.HasPrefix(cfg.Database, "lifecycle_upgrade_test") {
		t.Fatal("requires disposable loopback fixture")
	}
	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var version, count int
	if err = db.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,(SELECT count(*) FROM pg_tables WHERE schemaname='public')`).Scan(&version, &count); err != nil || version < 170000 || version >= 180000 || count != 0 {
		t.Fatal("requires empty PostgreSQL17 fixture", err)
	}
	_, err = db.Exec(ctx, `CREATE TABLE users(uuid uuid PRIMARY KEY,password text,proxy_uuid uuid);
CREATE TABLE finance_operations(id uuid PRIMARY KEY);
CREATE TABLE billing_ledger(id uuid PRIMARY KEY,amount numeric);
CREATE TABLE schema_migrations(version bigint PRIMARY KEY,dirty boolean NOT NULL);
INSERT INTO schema_migrations VALUES(2026101902,false);
INSERT INTO users VALUES('00000000-0000-0000-0000-000000000001','fixture-password','00000000-0000-0000-0000-000000000002');
INSERT INTO billing_ledger VALUES('00000000-0000-0000-0000-000000000003',9007199254740993.125);`)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func() string {
		var value string
		if e := db.QueryRow(ctx, `SELECT md5((SELECT string_agg(to_jsonb(u)::text,'' ORDER BY uuid) FROM users u)||(SELECT string_agg(to_jsonb(l)::text,'' ORDER BY id) FROM billing_ledger l))`).Scan(&value); e != nil {
			t.Fatal(e)
		}
		return value
	}
	before := fingerprint()
	raw, err := os.ReadFile("../../sql/migrations/2026102001_subscription_lifecycle.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"2026101902_previous.up.sql": {Data: []byte("SELECT 1;")}, "2026102001_subscription_lifecycle.up.sql": {Data: raw}}
	options := ControlledOptions{ExpectedVersion: 2026101902, TargetVersion: 2026102001, SHA256: checksum(string(raw)), LockTimeout: time.Second, StatementTimeout: time.Minute}
	bad := options
	bad.SHA256 = strings.Repeat("0", 64)
	if ControlledMigrate(ctx, dsn, files, bad) == nil {
		t.Fatal("wrong checksum admitted")
	}
	// Fail after CREATE TABLE: the transaction must remove DDL and retain old version.
	if _, err = db.Exec(ctx, `CREATE INDEX subscription_lifecycle_due_idx ON users(uuid)`); err != nil {
		t.Fatal(err)
	}
	if ControlledMigrate(ctx, dsn, files, options) == nil {
		t.Fatal("conflicting index admitted")
	}
	var relation *string
	if err = db.QueryRow(ctx, `SELECT to_regclass('public.subscription_lifecycle_states')::text`).Scan(&relation); err != nil || relation != nil {
		t.Fatal("failed migration leaked new table", err)
	}
	if _, err = db.Exec(ctx, `DROP INDEX subscription_lifecycle_due_idx`); err != nil {
		t.Fatal(err)
	}
	if err = ControlledMigrate(ctx, dsn, files, options); err != nil {
		t.Fatal(err)
	}
	if err = ControlledMigrate(ctx, dsn, files, options); err != nil {
		t.Fatal("safe rerun failed", err)
	}
	if fingerprint() != before {
		t.Fatal("migration reset identity or financial data")
	}
	if err = db.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != 2026102001 {
		t.Fatal("migration version not committed", err)
	}
	backward := options
	backward.ExpectedVersion = 2026102001
	backward.TargetVersion = 2026101902
	if ControlledMigrate(ctx, dsn, files, backward) == nil {
		t.Fatal("destructive downgrade admitted")
	}
}
