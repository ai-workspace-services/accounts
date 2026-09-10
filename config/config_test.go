package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSupabaseConnectURIOverridesFileDSN(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "account.yaml")
	configData := []byte("store:\n  driver: postgres\n  dsn: postgres://vps.example/account\n")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	t.Setenv("SUPABASE_CONNECT_URI", "postgres://supabase.example/account")
	t.Setenv("SUPABASE_CONNECT_URL", "postgres://legacy-alias.example/account")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got, want := cfg.Store.DSN, "postgres://supabase.example/account"; got != want {
		t.Fatalf("Store.DSN = %q, want %q", got, want)
	}
}

func TestLoadSupabaseConnectURLIsTransitionAlias(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "account.yaml")
	if err := os.WriteFile(configPath, []byte("store:\n  dsn: postgres://vps.example/account\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "postgres://supabase-alias.example/account")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got, want := cfg.Store.DSN, "postgres://supabase-alias.example/account"; got != want {
		t.Fatalf("Store.DSN = %q, want %q", got, want)
	}
}

func TestLoadDatabaseURLOverridesFileDSN(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "account.yaml")
	if err := os.WriteFile(configPath, []byte("store:\n  dsn: postgres://vps.example/account\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "")
	t.Setenv("DATABASE_URL", "postgres://business.example/account")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got, want := cfg.Store.DSN, "postgres://business.example/account"; got != want {
		t.Fatalf("Store.DSN = %q, want %q", got, want)
	}
}

func TestLoadCloudRunConnectionPoolOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "account.yaml")
	configData := []byte("store:\n  maxOpenConns: 30\n  maxIdleConns: 10\n")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	t.Setenv("DB_MAX_OPEN_CONNS", "3")
	t.Setenv("DB_MAX_IDLE_CONNS", "1")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Store.MaxOpenConns != 3 || cfg.Store.MaxIdleConns != 1 {
		t.Fatalf("store pool = (%d, %d), want (3, 1)", cfg.Store.MaxOpenConns, cfg.Store.MaxIdleConns)
	}
}

func TestLoadCloudRunIdlePoolIsCappedByOpenPool(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "account.yaml")
	if err := os.WriteFile(configPath, []byte("store:\n  maxOpenConns: 30\n  maxIdleConns: 10\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	t.Setenv("DB_MAX_OPEN_CONNS", "2")
	t.Setenv("DB_MAX_IDLE_CONNS", "8")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Store.MaxOpenConns != 2 || cfg.Store.MaxIdleConns != 2 {
		t.Fatalf("store pool = (%d, %d), want (2, 2)", cfg.Store.MaxOpenConns, cfg.Store.MaxIdleConns)
	}
}
