package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewedTargetMigrationRequiresOnePendingFileAndExactChecksum(t *testing.T) {
	dir := t.TempDir()
	content := []byte("-- fixture migration\nCREATE TABLE public.fixture_marker(id integer);\n")
	if err := os.WriteFile(filepath.Join(dir, "2026092801_fixture.up.sql"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	options := UpgradeOptions{
		ExpectedVersion:  2026092703,
		TargetVersion:    2026092801,
		MigrationSHA256:  hex.EncodeToString(digest[:]),
		LockTimeout:      time.Second,
		StatementTimeout: time.Minute,
	}
	migrations := []*migrationFile{{version: options.TargetVersion, name: "2026092801_fixture.up.sql"}}
	if _, err := reviewedTargetMigration(dir, migrations, options); err != nil {
		t.Fatalf("valid reviewed migration rejected: %v", err)
	}

	for name, mutate := range map[string]func(*UpgradeOptions){
		"wrong checksum": func(value *UpgradeOptions) {
			value.MigrationSHA256 = strings.Repeat("a", 64)
		},
		"unexpected pending migration": func(*UpgradeOptions) {},
	} {
		t.Run(name, func(t *testing.T) {
			value := options
			valueMigrations := append([]*migrationFile(nil), migrations...)
			if name == "unexpected pending migration" {
				valueMigrations = append(valueMigrations, &migrationFile{version: 2026092802, name: "2026092802_later.up.sql"})
			} else {
				mutate(&value)
			}
			if _, err := reviewedTargetMigration(dir, valueMigrations, value); err == nil {
				t.Fatal("unsafe reviewed migration unexpectedly accepted")
			}
		})
	}
}

func TestValidateUpgradeOptionsRejectsUnboundedRequests(t *testing.T) {
	runner := NewRunner(t.TempDir())
	valid := UpgradeOptions{
		ExpectedVersion:  2026092703,
		TargetVersion:    2026092801,
		MigrationSHA256:  strings.Repeat("a", 64),
		LockTimeout:      time.Second,
		StatementTimeout: time.Minute,
	}
	if err := runner.validateUpgradeOptions(valid); err != nil {
		t.Fatalf("valid bounded options rejected: %v", err)
	}
	for name, mutate := range map[string]func(*UpgradeOptions){
		"same version":              func(value *UpgradeOptions) { value.TargetVersion = value.ExpectedVersion },
		"uppercase checksum":        func(value *UpgradeOptions) { value.MigrationSHA256 = strings.Repeat("A", 64) },
		"missing lock timeout":      func(value *UpgradeOptions) { value.LockTimeout = 0 },
		"missing statement timeout": func(value *UpgradeOptions) { value.StatementTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if err := runner.validateUpgradeOptions(value); err == nil {
				t.Fatal("unsafe bounded options unexpectedly accepted")
			}
		})
	}
}
