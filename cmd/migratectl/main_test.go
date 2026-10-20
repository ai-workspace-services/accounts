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

func TestControlledMigrateUsesEnvironmentDSNOnly(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"controlled-migrate"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Flags().Lookup("dsn") != nil {
		t.Fatal("controlled-migrate must not accept a DSN flag")
	}
	for _, name := range []string{"expected-version", "target-version", "sha256", "lock-timeout", "statement-timeout"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing --%s flag", name)
		}
	}
}

func TestReconcileUsageCommandUsesExplicitGenerations(t *testing.T) {
	cmd := newUsageMergeCmd()
	if strings.Contains(cmd.Short, "V2") || strings.Contains(cmd.Short, "V3") {
		t.Fatalf("reconcile-usage help must not hardcode a generation pair: %q", cmd.Short)
	}
	for _, name := range []string{"source-dsn-env", "target-dsn-env", "source-generation", "target-generation", "schema-sha256", "billing-schema-sha256", "expected-target-version", "apply", "writers-paused", "expected-plan-sha256"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing --%s flag", name)
		}
	}
	for _, name := range []string{"source-dsn-env", "target-dsn-env"} {
		usage := cmd.Flags().Lookup(name).Usage
		if strings.Contains(usage, "V2") || strings.Contains(usage, "V3") {
			t.Errorf("--%s help must describe a generic source/destination: %q", name, usage)
		}
	}
	for name, example := range map[string]string{"source-generation": "prod-v3", "target-generation": "prod-v4"} {
		usage := cmd.Flags().Lookup(name).Usage
		if !strings.Contains(usage, example) {
			t.Errorf("--%s should show a generic adjacent-generation example, got %q", name, usage)
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
