package migrate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5"
	"testing/fstest"
)

func controlledMigratePostgresIntegration(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test PostgreSQL DSN")
	}
	admin, err := pgx.ConnectConfig(ctx, base.Copy())
	if err != nil {
		t.Fatal("unable to connect to disposable PostgreSQL test database")
	}
	defer admin.Close(context.Background())
	config := base.Copy()
	scopedDSN := dsn

	firstSQL := "BEGIN; CREATE TABLE controlled_probe (value text NOT NULL); INSERT INTO controlled_probe(value) VALUES ('applied'); COMMIT;"
	secondSQL := "BEGIN; CREATE TABLE should_rollback (id integer); SELECT pg_sleep(2); INSERT INTO deliberately_missing_table(id) VALUES (1); COMMIT;"
	thirdSQL := "SELECT 3;"
	files := fstest.MapFS{
		"0000000001_bootstrap.up.sql": {Data: []byte(firstSQL)},
		"0000000002_failure.up.sql":   {Data: []byte(secondSQL)},
		"0000000003_next.up.sql":      {Data: []byte(thirdSQL)},
	}
	options := ControlledOptions{ExpectedVersion: 0, TargetVersion: 1, SHA256: checksum(firstSQL), LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err == nil {
		t.Fatal("missing schema_migrations table was initialized implicitly")
	}
	var migrationTableExists, historyTableExists, probeTableExists bool
	if err := admin.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations') IS NOT NULL").Scan(&migrationTableExists); err != nil {
		t.Fatal("unable to inspect absent-table rejection")
	}
	if err := admin.QueryRow(ctx, "SELECT to_regclass('public."+controlledHistoryTable+"') IS NOT NULL").Scan(&historyTableExists); err != nil {
		t.Fatal("unable to inspect checksum-history rejection")
	}
	if err := admin.QueryRow(ctx, "SELECT to_regclass('public.controlled_probe') IS NOT NULL").Scan(&probeTableExists); err != nil {
		t.Fatal("unable to inspect SQL rejection")
	}
	if migrationTableExists || historyTableExists || probeTableExists {
		t.Fatal("missing-version-state rejection left database objects behind")
	}
	if _, err := admin.Exec(ctx, "CREATE TABLE public.schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)"); err != nil {
		t.Fatal("unable to seed version-zero test fixture")
	}
	if _, err := admin.Exec(ctx, "INSERT INTO public.schema_migrations(version, dirty) VALUES (0, false)"); err != nil {
		t.Fatal("unable to seed version-zero test fixture")
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := ControlledMigrate(canceledCtx, scopedDSN, files, options); err == nil {
		t.Fatal("canceled migration context was accepted")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err != nil {
		t.Fatalf("first controlled migration failed: %v", err)
	}
	var databaseName string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatal("unable to read disposable database name")
	}
	lockID, err := database.GenerateAdvisoryLockId(databaseName, "public", "schema_migrations")
	if err != nil {
		t.Fatal("unable to calculate test advisory lock")
	}
	var lockIDInt64 int64
	if _, err := fmt.Sscan(lockID, &lockIDInt64); err != nil {
		t.Fatal("unable to parse test advisory lock")
	}
	lockConn, err := pgx.ConnectConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("unable to connect for lock-timeout case")
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockIDInt64); err != nil {
		t.Fatal("unable to hold test advisory lock")
	}
	shortLock := ControlledOptions{ExpectedVersion: 1, TargetVersion: 2, SHA256: checksum(secondSQL), LockTimeout: 100 * time.Millisecond, StatementTimeout: 3 * time.Second}
	if err := ControlledMigrate(ctx, scopedDSN, files, shortLock); err == nil {
		t.Fatal("migration did not time out while waiting for the advisory lock")
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockIDInt64); err != nil {
		t.Fatal("unable to release test advisory lock")
	}
	if err := lockConn.Close(context.Background()); err != nil {
		t.Fatal("unable to close lock test connection")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, ControlledOptions{ExpectedVersion: 2, TargetVersion: 3, SHA256: checksum(thirdSQL), LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}); err == nil {
		t.Fatal("database version mismatch was accepted")
	}
	historyConn, err := pgx.ConnectConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("unable to inspect disposable PostgreSQL migration history")
	}
	if _, err := historyConn.Exec(ctx, "UPDATE public."+controlledHistoryTable+" SET sql_sha256 = $1 WHERE to_version = 1", strings.Repeat("0", 64)); err != nil {
		t.Fatal("unable to prepare checksum-mismatch replay case")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err == nil {
		t.Fatal("target-version replay without matching checksum history was accepted")
	}
	if _, err := historyConn.Exec(ctx, "UPDATE public."+controlledHistoryTable+" SET sql_sha256 = $1 WHERE to_version = 1", strings.ToLower(options.SHA256)); err != nil {
		t.Fatal("unable to restore test checksum history")
	}
	if _, err := historyConn.Exec(ctx, "UPDATE public.schema_migrations SET dirty = true"); err != nil {
		t.Fatal("unable to prepare dirty-state case")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, shortLock); err == nil {
		t.Fatal("dirty migration state was accepted")
	}
	if _, err := historyConn.Exec(ctx, "UPDATE public.schema_migrations SET dirty = false"); err != nil {
		t.Fatal("unable to restore test migration state")
	}
	if err := historyConn.Close(context.Background()); err != nil {
		t.Fatal("unable to close disposable PostgreSQL history connection")
	}
	shortStatement := ControlledOptions{ExpectedVersion: 1, TargetVersion: 2, SHA256: checksum(secondSQL), LockTimeout: 3 * time.Second, StatementTimeout: 100 * time.Millisecond}
	if err := ControlledMigrate(ctx, scopedDSN, files, shortStatement); err == nil {
		t.Fatal("migration statement did not time out")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err != nil {
		t.Fatalf("exact idempotent replay failed: %v", err)
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, ControlledOptions{ExpectedVersion: 0, TargetVersion: 2, SHA256: checksum(secondSQL), LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}); err == nil {
		t.Fatal("skipping the intermediate migration was accepted")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, shortLock); err == nil {
		t.Fatal("failing migration unexpectedly succeeded")
	}

	check, err := pgx.ConnectConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("unable to inspect disposable PostgreSQL test schema")
	}
	defer check.Close(context.Background())
	var version uint64
	var dirty bool
	if err := check.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		t.Fatal("migration version state was not persisted")
	}
	if version != 1 || dirty {
		t.Fatalf("version state = %d dirty=%v, want 1/false", version, dirty)
	}
	var value string
	if err := check.QueryRow(ctx, "SELECT value FROM controlled_probe").Scan(&value); err != nil || value != "applied" {
		t.Fatal("migration data was not committed")
	}
	var historyCount int
	if err := check.QueryRow(ctx, "SELECT count(*) FROM accounts_controlled_migration_history WHERE to_version = 1 AND sql_sha256 = $1", strings.ToLower(options.SHA256)).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatal("checksum history was not persisted")
	}
	var rolledBack bool
	if err := check.QueryRow(ctx, "SELECT to_regclass('should_rollback') IS NULL").Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatal("failed migration DDL was not rolled back")
	}
	var testTableExists bool
	if err := check.QueryRow(ctx, "SELECT to_regclass('accounts_controlled_migration_history') IS NOT NULL").Scan(&testTableExists); err != nil || !testTableExists {
		t.Fatal("controlled checksum history table was not created after explicit version seeding")
	}
	if err := check.QueryRow(ctx, "SELECT count(*) FROM accounts_controlled_migration_history").Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatal("failed or timed out migrations changed checksum history")
	}
}
