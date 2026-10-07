package config

import (
	"account/internal/dbruntime"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseRoleConfigUsesOnlyRuntimeDSN(t *testing.T) {
	dsn := "postgres://fixture:synthetic@localhost:5432/account?sslmode=disable"
	identity, _ := dbruntime.ConnectionIdentity(dsn)
	t.Setenv("DATABASE_RUNTIME_ROLE", "primary")
	t.Setenv("DATABASE_BACKGROUND_WRITERS", "false")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DATABASE_IDENTITY_SHA256", identity)
	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "")
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if os.WriteFile(path, []byte("mode: server-agent\nstore:\n  driver: postgres\n  dsn: stale-file-dsn\n"), 0o600) != nil {
		t.Fatal("fixture unavailable")
	}
	cfg, err := Load(path)
	if err != nil || cfg.Store.DSN != dsn || !cfg.Store.SchemaManaged || cfg.DatabaseRuntime.Role != "primary" || cfg.DatabaseRuntime.MayRunBackgroundWriters() {
		t.Fatal("managed configuration differs")
	}
	t.Setenv("SUPABASE_CONNECT_URI", dsn)
	if _, err := Load(path); err == nil {
		t.Fatal("alias was accepted despite explicit DATABASE_URL")
	}
}
