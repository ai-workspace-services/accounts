package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
)

const defaultDir = "account/sql/migrations"

// Runner coordinates golang-migrate operations.
type Runner struct {
	Dir string
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
