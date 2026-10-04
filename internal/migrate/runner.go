package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

const defaultDir = "account/sql/migrations"

// Runner coordinates golang-migrate operations.
type Runner struct {
	Dir string
}

// UpgradeOptions describes one reviewed, bounded migration step. The regular
// Up method remains available for local development; release automation must
// use Upgrade so a moving migration directory can never apply an unintended
// later migration.
type UpgradeOptions struct {
	ExpectedVersion  uint
	TargetVersion    uint
	MigrationSHA256  string
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

// NewRunner creates a runner that reads migration files from dir. When dir is
// empty, the default directory under account/sql/migrations is used.
func NewRunner(dir string) *Runner {
	if dir == "" {
		dir = defaultDir
	}
	return &Runner{Dir: dir}
}

// Up executes all migrations that have not been applied yet. Each step logs
// its outcome to provide clear visibility.
func (r *Runner) Up(ctx context.Context, dsn string) error {
	absDir, err := filepath.Abs(r.Dir)
	if err != nil {
		return err
	}

	migrations, err := r.loadMigrations(absDir)
	if err != nil {
		return err
	}

	m, err := migrate.New(fmt.Sprintf("file://%s", absDir), dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	currentVersion, dirty, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			currentVersion = 0
		} else {
			return fmt.Errorf("fetch current version: %w", err)
		}
	}

	if dirty {
		return fmt.Errorf("database is in a dirty state at version %d; please fix manually", currentVersion)
	}

	applied := false
	for _, migration := range migrations {
		if migration.version <= currentVersion {
			continue
		}

		fmt.Printf("→ Applying migration %s ...\n", migration.name)
		if err := m.Migrate(migration.version); err != nil {
			if errors.Is(err, migrate.ErrNoChange) {
				fmt.Printf("✅ Migration %s already applied\n", migration.name)
				continue
			}
			return fmt.Errorf("apply migration %s: %w", migration.name, err)
		}
		applied = true
		fmt.Printf("✅ Migration %s applied\n", migration.name)
	}

	if !applied {
		fmt.Println("✅ Database schema already up-to-date")
	}

	return nil
}

// Upgrade applies exactly one reviewed migration while holding a database
// advisory lock. It is deliberately fail-closed: dirty state, a version
// mismatch, an unexpected pending migration, a checksum mismatch, or a
// timeout-budget omission prevents any SQL from being applied.
func (r *Runner) Upgrade(ctx context.Context, dsn string, options UpgradeOptions) error {
	if err := r.validateUpgradeOptions(options); err != nil {
		return err
	}
	absDir, err := filepath.Abs(r.Dir)
	if err != nil {
		return err
	}
	migrations, err := r.loadMigrations(absDir)
	if err != nil {
		return err
	}
	target, err := reviewedTargetMigration(absDir, migrations, options)
	if err != nil {
		return err
	}

	lockDB, err := openDB(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open migration lock connection: %w", err)
	}
	defer lockDB.Close()
	lockContext, cancelLock := context.WithTimeout(ctx, options.LockTimeout)
	defer cancelLock()
	lockConn, err := lockDB.Conn(lockContext)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(lockContext,
		"SELECT pg_advisory_lock($1::bigint)", migrationAdvisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(),
			"SELECT pg_advisory_unlock($1::bigint)", migrationAdvisoryLockKey)
	}()

	boundedDSN, err := dsnWithTimeouts(dsn, options)
	if err != nil {
		return err
	}
	m, err := migrate.New(fmt.Sprintf("file://%s", absDir), boundedDSN)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	currentVersion, dirty, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			currentVersion = 0
		} else {
			return fmt.Errorf("fetch current version: %w", err)
		}
	}
	if dirty {
		return fmt.Errorf("database is in a dirty state at version %d; please fix manually", currentVersion)
	}
	if currentVersion > options.TargetVersion {
		return fmt.Errorf("database version %d is newer than reviewed target %d", currentVersion, options.TargetVersion)
	}
	if currentVersion != options.ExpectedVersion && currentVersion != options.TargetVersion {
		return fmt.Errorf("database version %d does not match reviewed expected %d or already-applied target %d",
			currentVersion, options.ExpectedVersion, options.TargetVersion)
	}
	if currentVersion == options.TargetVersion {
		return nil
	}
	if currentVersion != options.ExpectedVersion {
		return fmt.Errorf("database version %d is not the reviewed migration starting point", currentVersion)
	}
	if err := m.Migrate(target.version); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			return nil
		}
		return fmt.Errorf("apply reviewed migration %s: %w", target.name, err)
	}
	version, dirty, err := m.Version()
	if err != nil {
		return fmt.Errorf("verify migration result: %w", err)
	}
	if dirty || version != options.TargetVersion {
		return fmt.Errorf("migration finished at version %d dirty=%t; expected clean version %d", version, dirty, options.TargetVersion)
	}
	return nil
}

const migrationAdvisoryLockKey int64 = 2026092801

var migrationSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (r *Runner) validateUpgradeOptions(options UpgradeOptions) error {
	if options.ExpectedVersion == 0 || options.TargetVersion <= options.ExpectedVersion {
		return errors.New("expected and target migration versions must be positive and increasing")
	}
	if !migrationSHA256.MatchString(options.MigrationSHA256) {
		return errors.New("migration checksum must be a lowercase SHA-256 digest")
	}
	if options.LockTimeout <= 0 || options.StatementTimeout <= 0 {
		return errors.New("lock and statement timeouts must be positive")
	}
	return nil
}

func reviewedTargetMigration(absDir string, migrations []*migrationFile, options UpgradeOptions) (*migrationFile, error) {
	var target *migrationFile
	pending := 0
	for _, migration := range migrations {
		if migration.version == options.TargetVersion {
			target = migration
		}
		if migration.version > options.ExpectedVersion {
			pending++
			if migration.version != options.TargetVersion {
				return nil, fmt.Errorf("reviewed migration boundary contains unexpected pending version %d", migration.version)
			}
		}
	}
	if target == nil {
		return nil, fmt.Errorf("reviewed target migration %d is missing", options.TargetVersion)
	}
	if pending != 1 {
		return nil, fmt.Errorf("reviewed migration boundary requires exactly one pending migration, found %d", pending)
	}
	content, err := os.ReadFile(filepath.Join(absDir, target.name))
	if err != nil {
		return nil, fmt.Errorf("read reviewed migration %s: %w", target.name, err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != options.MigrationSHA256 {
		return nil, fmt.Errorf("reviewed migration %s checksum differs from the requested digest", target.name)
	}
	return target, nil
}

func dsnWithTimeouts(dsn string, options UpgradeOptions) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("bounded migration requires a URL-form PostgreSQL DSN")
	}
	query := parsed.Query()
	query.Set("options", fmt.Sprintf("-c lock_timeout=%dms -c statement_timeout=%dms",
		options.LockTimeout.Milliseconds(), options.StatementTimeout.Milliseconds()))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// Version reports the current schema version tracked by golang-migrate.
func (r *Runner) Version(dsn string) (uint, bool, error) {
	absDir, err := filepath.Abs(r.Dir)
	if err != nil {
		return 0, false, err
	}

	m, err := migrate.New(fmt.Sprintf("file://%s", absDir), dsn)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m)

	version, dirty, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			return 0, false, nil
		}
		return 0, false, err
	}

	return version, dirty, nil
}

// Reset is retained for CLI compatibility but destructive resets are disabled.
// Production and UAT databases must be upgraded with forward-only migrations.
func (r *Runner) Reset(ctx context.Context, dsn string) error {
	return errors.New("database reset is disabled because it would delete retained account and billing history; apply forward-only migrations with migratectl migrate")
}

func (r *Runner) loadMigrations(absDir string) ([]*migrationFile, error) {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, err
	}

	migrationMap := make(map[uint]*migrationFile)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		parts := strings.SplitN(name, "_", 2)
		if len(parts) != 2 {
			continue
		}
		version, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		migrationMap[uint(version)] = &migrationFile{
			version: uint(version),
			name:    name,
		}
	}

	var migrations []*migrationFile
	for _, m := range migrationMap {
		migrations = append(migrations, m)
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

func closeMigrator(m *migrate.Migrate) {
	if m == nil {
		return
	}
	_, _ = m.Close()
}

type migrationFile struct {
	version uint
	name    string
}
