package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	accountmigrations "account/sql/migrations"
	"github.com/jackc/pgx/v5"
)

func TestSplitMigrationSQLStripsOnlyOuterTransactionWrapper(t *testing.T) {
	sql := "-- reviewed migration\nBEGIN;\nCREATE TABLE example (value text);\nDO $$ BEGIN PERFORM 'COMMIT;'; END $$;\nCOMMIT;"
	statements, err := splitMigrationSQL(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 {
		t.Fatalf("got %d statements, want 2: %#v", len(statements), statements)
	}
	if !strings.Contains(statements[1], "'COMMIT;'") {
		t.Fatalf("procedural body was altered: %s", statements[1])
	}
}

func TestSplitMigrationSQLRejectsIncompleteOrNestedTransactionControl(t *testing.T) {
	for _, sql := range []string{
		"BEGIN; CREATE TABLE example(id int);",
		"CREATE TABLE example(id int); COMMIT;",
		"CREATE TABLE example(id int); BEGIN;",
		"CREATE TABLE example(id int); ROLLBACK;",
	} {
		if _, err := splitMigrationSQL(sql); err == nil {
			t.Errorf("expected transaction-control rejection for %q", sql)
		}
	}
}

func TestSplitMigrationSQLIgnoresTransactionWordsInStringsAndComments(t *testing.T) {
	sql := "/* BEGIN; */ INSERT INTO examples(value) VALUES ('COMMIT;'); -- ROLLBACK;\n"
	statements, err := splitMigrationSQL(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 || !strings.Contains(statements[0], "'COMMIT;'") {
		t.Fatalf("unexpected statements: %#v", statements)
	}
}

func TestSplitMigrationSQLEscapedStringDoesNotExposeTransactionKeywords(t *testing.T) {
	sql := `BEGIN; SELECT E'escaped \' COMMIT; text'; SELECT 2; COMMIT;`
	statements, err := splitMigrationSQL(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 {
		t.Fatalf("got %d statements, want 2: %#v", len(statements), statements)
	}
}

func TestEveryEmbeddedMigrationHasSupportedTransactionWrapper(t *testing.T) {
	entries, err := accountmigrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		sql, err := accountmigrations.Files.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := splitMigrationSQL(string(sql)); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
		}
	}
}

func TestControlledMigrateRejectsSkippedVersionAndBadChecksumBeforeConnecting(t *testing.T) {
	files := fstest.MapFS{
		"0000000010_first.up.sql":  {Data: []byte("SELECT 10;\n")},
		"0000000020_second.up.sql": {Data: []byte("SELECT 20;\n")},
		"0000000030_third.up.sql":  {Data: []byte("SELECT 30;\n")},
	}
	if err := ControlledMigrate(t.Context(), "postgres://secret@invalid/test", files, ControlledOptions{
		ExpectedVersion: 10, TargetVersion: 30, SHA256: checksum("SELECT 30;\n"), LockTimeout: time.Second, StatementTimeout: time.Second,
	}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("skipped migration should fail safely before connection: %v", err)
	}
	if err := ControlledMigrate(t.Context(), "postgres://secret@invalid/test", files, ControlledOptions{
		ExpectedVersion: 20, TargetVersion: 30, SHA256: checksum("SELECT 31;\n"), LockTimeout: time.Second, StatementTimeout: time.Second,
	}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("checksum mismatch should fail safely before connection: %v", err)
	}
}

func TestControlledMigrateRejectsUnboundedTimeoutBeforeConnecting(t *testing.T) {
	files := fstest.MapFS{"0000000010_first.up.sql": {Data: []byte("SELECT 10;\n")}}
	if err := ControlledMigrate(t.Context(), "postgres://secret@invalid/test", files, ControlledOptions{
		ExpectedVersion: 0, TargetVersion: 10, SHA256: checksum("SELECT 10;\n"), LockTimeout: MaxControlledLockTimeout + time.Nanosecond, StatementTimeout: time.Second,
	}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unbounded lock timeout should fail safely before connection: %v", err)
	}
	if err := ControlledMigrate(t.Context(), "postgres://secret@invalid/test", files, ControlledOptions{
		ExpectedVersion: 0, TargetVersion: 10, SHA256: checksum("SELECT 10;\n"), LockTimeout: time.Second, StatementTimeout: MaxControlledStatementTimeout + time.Nanosecond,
	}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unbounded statement timeout should fail safely before connection: %v", err)
	}
}

func TestControlledMigratePostgresIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("ACCOUNTS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		if os.Getenv("ACCOUNTS_TEST_POSTGRES_DSN_REQUIRED") == "1" {
			t.Fatal("ACCOUNTS_TEST_POSTGRES_DSN is required for this integration run")
		}
		t.Skip("set ACCOUNTS_TEST_POSTGRES_DSN to a disposable PostgreSQL database whose name contains 'test'")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test PostgreSQL DSN")
	}
	if !strings.Contains(strings.ToLower(config.Database), "test") || !isLocalDatabaseHost(config.Host) {
		t.Fatal("integration tests require a local disposable PostgreSQL database whose name contains 'test'")
	}
	controlledMigratePostgresIntegration(t, dsn)
}

func isLocalDatabaseHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checksum(sql string) string {
	h := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(h[:])
}
