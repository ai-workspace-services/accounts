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
