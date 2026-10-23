package migrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type migrationOwnerFixture struct {
	os.FileInfo
	uid uint32
}

func (f migrationOwnerFixture) Sys() any { return &syscall.Stat_t{Uid: f.uid} }
func TestMigrationBackupFileOwnership(t *testing.T) {
	for _, test := range []struct {
		owner   uint32
		service int
		allowed bool
	}{{0, 0, true}, {0, 501, true}, {501, 501, true}, {502, 501, false}, {501, 0, false}} {
		if migrationTrustedOwner(migrationOwnerFixture{uid: test.owner}, test.service) != test.allowed {
			t.Fatal("backup owner boundary", test)
		}
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if migrationTrustedBackupParents(filepath.Join(dir, "proof.json")) == nil {
		t.Fatal("group-writable backup parent accepted")
	}
}

func TestMigrationAPIBackupGate(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "verified-backup.json")
	if _, err := MigrationBackupFileGate(path); err == nil {
		t.Fatal("missing backup proof accepted")
	}
	now := time.Now().UTC()
	hash := strings.Repeat("a", 64)
	receipt := MigrationBackupReceipt{Format: 1, BackupID: "isolated-fixture", BackupSHA256: hash, TargetIdentitySHA256: hash, TargetSchemaSHA256: hash, TargetBaselineSHA256: hash, RestoredBaselineSHA256: hash, RestoreVerified: true, VerifiedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	write := func(value MigrationBackupReceipt) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(receipt)
	gate, err := MigrationBackupFileGate(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := MigrationPlan{TargetIdentitySHA256: hash, TargetSchemaSHA256: hash, TargetBaselineSHA256: hash}
	if err = gate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	other := plan
	other.TargetBaselineSHA256 = strings.Repeat("b", 64)
	if err = gate(context.Background(), other); err == nil {
		t.Fatal("backup baseline mismatch accepted")
	}
	expired := receipt
	expired.ExpiresAt = now.Add(-time.Second)
	write(expired)
	if err = gate(context.Background(), plan); err == nil {
		t.Fatal("expired proof accepted after startup")
	}
	invalid := receipt
	invalid.RestoreVerified = false
	write(invalid)
	if _, err = MigrationBackupFileGate(path); err == nil {
		t.Fatal("unverified restore accepted")
	}
	invalid = receipt
	invalid.RestoredBaselineSHA256 = strings.Repeat("b", 64)
	write(invalid)
	if _, err = MigrationBackupFileGate(path); err == nil {
		t.Fatal("different restored content accepted")
	}
	write(receipt)
	os.Chmod(path, 0644)
	if _, err = MigrationBackupFileGate(path); err == nil {
		t.Fatal("nonprivate proof accepted")
	}
	os.Chmod(path, 0600)
	link := filepath.Join(t.TempDir(), "link")
	if os.Symlink(path, link) != nil {
		t.Fatal("fixture symlink failed")
	}
	if _, err = MigrationBackupFileGate(link); err == nil {
		t.Fatal("symlink proof accepted")
	}
}
