package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	migratelib "github.com/golang-migrate/migrate/v4"
)

func TestVersionRegistersFileSourceDriver(t *testing.T) {
	_, err := migratelib.New(fmt.Sprintf("file://%s", filepath.ToSlash(t.TempDir())), "unknown://")
	if err != nil {
		if strings.Contains(err.Error(), "unknown driver 'file'") {
			t.Fatalf("file source driver was not registered: %v", err)
		}
	}
}

func TestMigrateDSNEnvironmentContract(t *testing.T) {
	dir := t.TempDir()
	t.Run("requires a value", func(t *testing.T) {
		t.Setenv("MIGRATECTL_TEST_DSN", "")
		cmd := newMigrateCmd(&dir)
		cmd.SetArgs([]string{"--dsn-env", "MIGRATECTL_TEST_DSN"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--dsn is required") {
			t.Fatalf("empty DSN environment variable should fail closed, got %v", err)
		}
	})
	t.Run("rejects command line and environment DSNs together", func(t *testing.T) {
		t.Setenv("MIGRATECTL_TEST_DSN", "postgres://fixture")
		cmd := newMigrateCmd(&dir)
		cmd.SetArgs([]string{"--dsn", "postgres://fixture", "--dsn-env", "MIGRATECTL_TEST_DSN"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("ambiguous DSN sources should fail closed, got %v", err)
		}
	})
}
