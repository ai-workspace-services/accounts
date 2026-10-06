package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"account/config"
	"account/internal/dbruntime"
	"account/internal/migrate"
	schema "account/sql"
)

func managedFixtureConfig(t *testing.T, role, dsn string) *config.Config {
	t.Helper()
	identity, err := dbruntime.ConnectionIdentity(dsn)
	if err != nil {
		t.Fatal("fixture connection shape differs")
	}
	t.Setenv("DATABASE_RUNTIME_ROLE", role)
	t.Setenv("DATABASE_BACKGROUND_WRITERS", "false")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DATABASE_IDENTITY_SHA256", identity)
	t.Setenv("SUPABASE_CONNECT_URI", "")
	t.Setenv("SUPABASE_CONNECT_URL", "")
	t.Setenv("IMAGE", "ghcr.io/ai-workspace-services/accounts:sha-"+strings.Repeat("a", 40))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture listener unavailable")
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	file := filepath.Join(t.TempDir(), "runtime.yaml")
	if os.WriteFile(file, []byte("mode: server-agent\nserver:\n  addr: \""+addr+"\"\nstore:\n  driver: postgres\n"), 0o600) != nil {
		t.Fatal("fixture config unavailable")
	}
	cfg, err := config.Load(file)
	if err != nil {
		t.Fatal("fixture config refused")
	}
	return cfg
}

// Return only reviewed catalog diagnostics or SQLSTATE, never a driver message,
// connection string, user value or arbitrary startup error.
func managedFixtureFailure(err error) string {
	if err == nil {
		return "unexpected normal exit"
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		return "SQLSTATE " + pgError.Code
	}
	message := err.Error()
	if regexp.MustCompile(`^(native (runtime|target|public\.)|complete target visibility|cannot (verify|read complete|decode target)|incomplete business scope|managed runtime configuration|config is nil)`).MatchString(message) && regexp.MustCompile(`^[A-Za-z0-9_. -]{1,200}$`).MatchString(message) {
		return message
	}
	return "unclassified startup failure"
}

func startManagedFixture(t *testing.T, cfg *config.Config) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServerAndAgent(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("managed server returned an error")
			}
		case <-time.After(5 * time.Second):
			t.Error("managed server did not stop")
		}
	}
	base := "http://" + cfg.Server.Addr
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("managed server stopped before qualified probe: %s", managedFixtureFailure(err))
		default:
		}
		resp, err := client.Get(base + "/api/ping")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base, stop
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	t.Fatal("managed probe never became available")
	return "", stop
}

func TestManagedStandbyStartsWithoutDatabaseOrBundledAgent(t *testing.T) {
	// No database exists at this URI. Any accidental pool/daemon construction
	// blocks startup; this role must provide probes without reaching it.
	cfg := managedFixtureConfig(t, "standby", "postgres://fixture:synthetic@unreachable.invalid:5432/postgres?sslmode=require")
	base, stop := startManagedFixture(t, cfg)
	defer stop()
	for _, path := range []string{"/readyz", "/api/users", "/api/auth/login", "/api/agent/status"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal("standby request unavailable")
		}
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Fatal("standby admitted readiness or business request")
		}
	}
	resp, err := http.Get(base + "/api/ping")
	if err != nil {
		t.Fatal("standby probe unavailable")
	}
	defer resp.Body.Close()
	var body map[string]any
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body["commit"] != strings.Repeat("a", 40) || body["database_role"] != "standby" || body["background_writers"] != false || body["bootstrap_writes"] != false {
		t.Fatal("standby release/role metadata differs")
	}
	if runAgent(context.Background(), cfg, slog.Default()) == nil {
		t.Fatal("managed role enabled bundled agent")
	}
}

func TestManagedNativeStartupPreservesAllBusinessFacts(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_MANAGED_RUNTIME_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable managed runtime fixture not configured")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" ||
		(parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1") || parsed.Port() != "5432" || parsed.Path != "/account" || parsed.User.Username() != "postgres" {
		t.Fatal("fixture refuses non-disposable database connection")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("fixture database unavailable")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal("compiled artifact unavailable")
	}
	if _, err = migrate.InitializeNative(ctx, dsn, migrate.NativeInitOptions{Environment: "uat", SchemaSHA256: manifest.SchemaSHA256,
		WritersPaused: true, DryRun: false, LockTimeout: time.Second, StatementTimeout: 30 * time.Second}); err != nil {
		t.Fatal("fixture native init failed")
	}
	billing, err := os.ReadFile("../../internal/migrate/testdata/cloud_vendor_costs_native.sql")
	if err != nil {
		t.Fatal("fixture Billing SQL unavailable")
	}
	digest := sha256.Sum256(billing)
	if hex.EncodeToString(digest[:]) != migrate.FullBusinessBillingSHA256 {
		t.Fatal("fixture Billing SQL changed")
	}
	if _, err = db.ExecContext(ctx, string(billing)); err != nil {
		t.Fatal("fixture Billing schema failed")
	}
	if _, err = db.ExecContext(ctx, "UPDATE public.schema_migrations SET version=2026100701"); err != nil {
		t.Fatal("fixture version failed")
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid,active,role,groups,proxy_uuid_expires_at)
VALUES('00000000-0000-0000-0000-000000000101','preserved-sandbox','synthetic-password','sandbox@svc.plus','00000000-0000-0000-0000-000000000201',true,'admin','["preserved"]','2020-01-01T00:00:00Z'),
('00000000-0000-0000-0000-000000000102','preserved-review','synthetic-password','review@svc.plus','00000000-0000-0000-0000-000000000202',true,'admin','["preserved"]',NULL);
INSERT INTO public.billing_ledger(id,account_uuid,bucket_start,bucket_end,entry_type,rated_bytes,amount_delta,balance_after)
VALUES('00000000-0000-0000-0000-000000000301','00000000-0000-0000-0000-000000000101','2026-10-01T00:00:00Z','2026-10-01T00:01:00Z','usage',9007199254740993,0.125,12.75)`); err != nil {
		t.Fatal("synthetic facts seed failed")
	}
	if _, err = db.ExecContext(ctx, `CREATE ROLE runtime_readonly_ci LOGIN PASSWORD 'synthetic-runtime' NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS;
GRANT CONNECT ON DATABASE account TO runtime_readonly_ci;
GRANT USAGE ON SCHEMA public TO runtime_readonly_ci;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO runtime_readonly_ci`); err != nil {
		t.Fatal("fixture readonly runtime role failed")
	}
	// This is a disposable TARGET runtime role: SELECT-only, with full native
	// RLS visibility and no ownership/CREATE/DML. It is never a migration source
	// role; readonly_release remains NOBYPASSRLS with reviewed SELECT policies.
	// Full row and schema/control-catalog hashes. No raw rows leave the test.
	fingerprint := func() string {
		t.Helper()
		h := sha256.New()
		tables := append(append([]string{}, manifest.BusinessTables...), "cloud_vendor_costs", "schema_migrations")
		for _, name := range tables {
			var sha string
			query := `SELECT encode(digest(coalesce(string_agg(value,E'\n' ORDER BY value),''),'sha256'),'hex') FROM (SELECT to_jsonb(t)::text value FROM public."` + name + `" t) rows`
			if db.QueryRowContext(ctx, query).Scan(&sha) != nil {
				t.Fatal("fixture business fingerprint unavailable")
			}
			h.Write([]byte(name + sha))
		}
		var catalog string
		if db.QueryRowContext(ctx, `SELECT coalesce(string_agg(v,E'\n' ORDER BY v),'') FROM (
SELECT to_jsonb(c)::text v FROM information_schema.columns c WHERE table_schema='public'
UNION ALL SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace='public'::regnamespace
UNION ALL SELECT indexdef FROM pg_indexes WHERE schemaname='public'
UNION ALL SELECT pg_get_functiondef(p.oid) FROM pg_proc p WHERE pronamespace='public'::regnamespace AND prokind='f') catalog`).Scan(&catalog) != nil {
			t.Fatal("fixture schema fingerprint unavailable")
		}
		h.Write([]byte(catalog))
		return hex.EncodeToString(h.Sum(nil))
	}
	before := fingerprint()
	parsed.User = url.UserPassword("runtime_readonly_ci", "synthetic-runtime")
	cfg := managedFixtureConfig(t, "primary", parsed.String())
	base, stop := startManagedFixture(t, cfg)
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatal("native readiness unavailable")
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("native readonly startup not ready")
	}
	stop()
	if before != fingerprint() {
		t.Fatal("managed startup changed a business fact or schema")
	}
	readonlyDB, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal("readonly target fixture unavailable")
	}
	defer readonlyDB.Close()
	if _, err = db.ExecContext(ctx, "REVOKE SELECT ON public.users FROM runtime_readonly_ci"); err != nil {
		t.Fatal("fixture read privilege tamper failed")
	}
	if migrate.VerifyNativeRuntime(ctx, readonlyDB) == nil {
		t.Fatal("runtime accepted an unreadable business table")
	}
	if _, err = db.ExecContext(ctx, "GRANT SELECT ON public.users TO runtime_readonly_ci"); err != nil {
		t.Fatal("fixture read privilege restoration failed")
	}
	if err = migrate.VerifyNativeRuntime(ctx, db); err != nil {
		t.Fatal("exact native runtime refused")
	}
	if _, err = db.ExecContext(ctx, "ALTER TABLE public.users ADD COLUMN unreviewed_runtime_column integer"); err != nil {
		t.Fatal("fixture schema tamper failed")
	}
	if migrate.VerifyNativeRuntime(ctx, db) == nil {
		t.Fatal("unreviewed native schema accepted")
	}
	if _, err = db.ExecContext(ctx, "ALTER TABLE public.users DROP COLUMN unreviewed_runtime_column; UPDATE public.schema_migrations SET dirty=true"); err != nil {
		t.Fatal("fixture dirty version failed")
	}
	if migrate.VerifyNativeRuntime(ctx, db) == nil {
		t.Fatal("dirty native checkpoint accepted")
	}
}
