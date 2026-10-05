package migrate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
	schema := fmt.Sprintf("controlled_migrate_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal("unable to create test schema")
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}()

	config := base.Copy()
	config.RuntimeParams["search_path"] = schema
	scopedDSN := config.ConnString()

	firstSQL := "BEGIN; CREATE TABLE controlled_probe (value text NOT NULL); INSERT INTO controlled_probe(value) VALUES ('applied'); COMMIT;"
	secondSQL := "BEGIN; CREATE TABLE should_rollback (id integer); INSERT INTO deliberately_missing_table(id) VALUES (1); COMMIT;"
	files := fstest.MapFS{
		"0000000001_bootstrap.up.sql": {Data: []byte(firstSQL)},
		"0000000002_failure.up.sql":   {Data: []byte(secondSQL)},
	}
	options := ControlledOptions{ExpectedVersion: 0, TargetVersion: 1, SHA256: checksum(firstSQL), LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err != nil {
		t.Fatalf("first controlled migration failed: %v", err)
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err != nil {
		t.Fatalf("exact idempotent replay failed: %v", err)
	}
	historyConn, err := pgx.ConnectConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("unable to inspect disposable PostgreSQL migration history")
	}
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := historyConn.Exec(ctx, "UPDATE "+quotedSchema+".accounts_controlled_migration_history SET sql_sha256 = $1 WHERE to_version = 1", strings.Repeat("0", 64)); err != nil {
		t.Fatal("unable to prepare checksum-mismatch replay case")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, options); err == nil {
		t.Fatal("target-version replay without matching checksum history was accepted")
	}
	if _, err := historyConn.Exec(ctx, "UPDATE "+quotedSchema+".accounts_controlled_migration_history SET sql_sha256 = $1 WHERE to_version = 1", strings.ToLower(options.SHA256)); err != nil {
		t.Fatal("unable to restore test checksum history")
	}
	if err := historyConn.Close(context.Background()); err != nil {
		t.Fatal("unable to close disposable PostgreSQL history connection")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, ControlledOptions{ExpectedVersion: 0, TargetVersion: 2, SHA256: checksum(secondSQL), LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}); err == nil {
		t.Fatal("skipping the intermediate migration was accepted")
	}
	if err := ControlledMigrate(ctx, scopedDSN, files, ControlledOptions{ExpectedVersion: 1, TargetVersion: 2, SHA256: checksum(secondSQL), LockTimeout: 3 * time.Second, StatementTimeout: 3 * time.Second}); err == nil {
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
}
