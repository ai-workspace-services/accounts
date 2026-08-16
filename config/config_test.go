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
