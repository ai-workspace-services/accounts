package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Produced by the migration owner after an independent populated restore drill.
// This assertion is accepted only from a protected local owner-controlled file,
// never from an HTTP caller or GitOps declaration. The owner verifies artifacts.
type MigrationBackupReceipt struct {
	Format                 int       `json:"format"`
	BackupID               string    `json:"backup_id"`
	BackupSHA256           string    `json:"backup_sha256"`
	TargetIdentitySHA256   string    `json:"target_identity_sha256"`
	TargetSchemaSHA256     string    `json:"target_schema_sha256"`
	TargetBaselineSHA256   string    `json:"target_baseline_sha256"`
	RestoredBaselineSHA256 string    `json:"restored_baseline_sha256"`
	RestoreVerified        bool      `json:"restore_verified"`
	VerifiedAt             time.Time `json:"verified_at"`
	ExpiresAt              time.Time `json:"expires_at"`
}

type MigrationBackupGate func(context.Context, MigrationPlan) error

var migrationSHA = regexp.MustCompile(`^[a-f0-9]{64}$`)

func migrationTrustedOwner(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(uid))
}
func migrationTrustedBackupParents(path string) error {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !migrationTrustedOwner(info, os.Geteuid()) {
			return errors.New("backup receipt requires trusted non-symlink parent directories")
		}
		stat := info.Sys().(*syscall.Stat_t)
		// A root-owned sticky temporary root cannot replace another owner's private
		// child directory. All other writable shared ancestors are refused.
		if info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return errors.New("backup receipt parent is writable by another user")
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

func readMigrationBackupReceipt(path string) (MigrationBackupReceipt, error) {
	var receipt MigrationBackupReceipt
	if !filepath.IsAbs(path) {
		return receipt, errors.New("absolute migration-owner backup receipt path required")
	}
	if err := migrationTrustedBackupParents(path); err != nil {
		return receipt, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return receipt, errors.New("migration-owner backup receipt unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16<<10 || !migrationTrustedOwner(info, os.Geteuid()) {
		return receipt, errors.New("backup receipt requires private mode 0600 regular file")
	}
	decoder := json.NewDecoder(io.LimitReader(f, (16<<10)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
		return receipt, errors.New("invalid backup receipt")
	}
	now := time.Now().UTC()
	if receipt.Format != 1 || receipt.BackupID == "" || len(receipt.BackupID) > 128 || !receipt.RestoreVerified || receipt.VerifiedAt.After(now) || receipt.VerifiedAt.Before(now.Add(-24*time.Hour)) || !receipt.ExpiresAt.After(now) || receipt.ExpiresAt.After(receipt.VerifiedAt.Add(24*time.Hour)) {
		return receipt, errors.New("backup restore verification missing or expired")
	}
	for _, hash := range []string{receipt.BackupSHA256, receipt.TargetIdentitySHA256, receipt.TargetSchemaSHA256, receipt.TargetBaselineSHA256, receipt.RestoredBaselineSHA256} {
		if !migrationSHA.MatchString(hash) {
			return receipt, errors.New("backup proof fingerprint missing")
		}
	}
	if receipt.TargetBaselineSHA256 != receipt.RestoredBaselineSHA256 {
		return receipt, errors.New("backup restored facts differ")
	}
	return receipt, nil
}

func MigrationBackupFileGate(path string) (MigrationBackupGate, error) {
	if _, err := readMigrationBackupReceipt(path); err != nil {
		return nil, err
	}
	return func(ctx context.Context, plan MigrationPlan) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt, err := readMigrationBackupReceipt(path)
		if err != nil {
			return err
		}
		if receipt.TargetIdentitySHA256 != plan.TargetIdentitySHA256 || receipt.TargetSchemaSHA256 != plan.TargetSchemaSHA256 || receipt.TargetBaselineSHA256 != plan.TargetBaselineSHA256 {
			return errors.New("backup proof does not bind reviewed target baseline")
		}
		return nil
	}, nil
}
