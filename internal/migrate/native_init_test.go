package migrate

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	schema "account/sql"
	"github.com/jackc/pgx/v5"
)

func TestNativeInitRefusesInvalidContractBeforeDatabaseAccess(t *testing.T) {
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	valid := NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}
	cases := []NativeInitOptions{valid, valid, valid, valid, valid}
	cases[0].Environment = ""
	cases[1].SchemaSHA256 = strings.Repeat("0", 64)
	cases[2].WritersPaused = false
	cases[3].LockTimeout = 0
	cases[4].StatementTimeout = 6 * time.Minute
	for _, options := range cases {
		if _, err = InitializeNative(context.Background(), "invalid://contains-private-fixture", options); err == nil || strings.Contains(err.Error(), "private-fixture") || strings.Contains(err.Error(), "connection failed") {
			t.Fatalf("contract should stop before database access: %v", err)
		}
	}
}

func TestNativeInitPostgres17(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_NATIVE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL17 fixture not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "account" || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") {
		t.Fatal("native test requires a local isolated account database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	defer db.Close()
	var version int
	if err = db.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&version); err != nil || version < 170000 || version >= 180000 {
		t.Fatal("native test fixture must be PostgreSQL17")
	}
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	options := NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, DryRun: true, LockTimeout: time.Second, StatementTimeout: 30 * time.Second}
	ctx := context.Background()
	// Qualify broad empty classification before the native SQL can run.
	if _, err = db.Exec("CREATE SCHEMA native_test_hidden; CREATE TYPE native_test_hidden.example AS ENUM ('fixture')"); err != nil {
		t.Fatal(err)
	}
	if _, err = InitializeNative(ctx, dsn, options); err == nil {
		t.Fatal("hidden application type must refuse initialization")
	}
	if _, err = db.Exec("DROP SCHEMA native_test_hidden CASCADE; CREATE EXTENSION IF NOT EXISTS pgcrypto"); err != nil {
		t.Fatal(err)
	}
	receipt, err := InitializeNative(ctx, dsn, options)
	if err != nil || receipt.Result != "eligible" || receipt.DatabaseCutoverApproved {
		t.Fatalf("empty eligibility: %#v %v", receipt, err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema='public'").Scan(&count); err != nil || count != 0 {
		t.Fatal("dry-run changed fixture schema")
	}
	options.DryRun = false
	receipt, err = InitializeNative(ctx, dsn, options)
	if err != nil || receipt.Result != "initialized" || len(receipt.BusinessTables) != 52 || receipt.BusinessRows != 0 || receipt.DatabaseCutoverApproved {
		t.Fatalf("native init: %#v %v", receipt, err)
	}
	var migrationVersion uint
	var dirty bool
	if err = db.QueryRow("SELECT version,dirty FROM public.schema_migrations").Scan(&migrationVersion, &dirty); err != nil || migrationVersion != manifest.MigrationVersion || dirty {
		t.Fatal("native initialization did not establish clean latest migration version")
	}
	if _, err = db.Exec(`INSERT INTO public.users(uuid,username,password,email,proxy_uuid,email_verified_at) VALUES('00000000-0000-0000-0000-000000000101','native-fixture','fixture-password','native@example.invalid','00000000-0000-0000-0000-000000000201','2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal("native user fixture rejected")
	}
	if _, err = InitializeNative(ctx, dsn, options); err == nil {
		t.Fatal("repeated initialization must refuse the existing database")
	}
	var proxy string
	var verified bool
	if err = db.QueryRow("SELECT proxy_uuid::text,email_verified FROM public.users WHERE uuid='00000000-0000-0000-0000-000000000101'").Scan(&proxy, &verified); err != nil || proxy != "00000000-0000-0000-0000-000000000201" || !verified {
		t.Fatal("native fixture Proxy UUID or generated email verification changed")
	}
}
