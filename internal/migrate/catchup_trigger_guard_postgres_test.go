package migrate

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	schema "account/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// This creates only randomly named disposable databases and its own test
// roles in an explicitly provided loopback fixture, never in a target cluster.
func TestCatchupTriggerGuardsPostgres(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_API_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL fixture not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "postgres" || cfg.User != "postgres" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		t.Fatal("requires dedicated loopback PostgreSQL postgres fixture")
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	var exists bool
	if err = admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='accounts_migration')`).Scan(&exists); err != nil || exists {
		t.Fatal("executor role already exists or cannot be checked; refusing fixture")
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	name, business := "guard_test_"+suffix, "guard_business_"+suffix
	if _, err = admin.Exec(`CREATE DATABASE ` + quoteBusiness(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(`DROP DATABASE ` + quoteBusiness(name) + ` WITH (FORCE)`); e != nil {
			t.Errorf("fixture database cleanup: %v", e)
		}
		if _, e := admin.Exec(`DROP ROLE IF EXISTS ` + quoteBusiness(business) + `, accounts_migration`); e != nil {
			t.Errorf("fixture role cleanup: %v", e)
		}
	})
	if _, err = admin.Exec(`CREATE ROLE accounts_migration LOGIN PASSWORD 'fixture-executor' NOINHERIT NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS; CREATE ROLE ` + quoteBusiness(business) + ` LOGIN PASSWORD 'fixture-business' NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	dbCfg := cfg.Copy()
	dbCfg.Database = name
	db := stdlib.OpenDB(*dbCfg)
	db.SetMaxOpenConns(1)
	defer db.Close()
	native, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(native)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE public.guard_fixture(id integer PRIMARY KEY, updated_at timestamptz NOT NULL DEFAULT now(), version bigint NOT NULL DEFAULT 1);
CREATE TRIGGER guard_timestamp BEFORE UPDATE ON public.guard_fixture FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();
CREATE TRIGGER guard_version BEFORE UPDATE ON public.guard_fixture FOR EACH ROW EXECUTE FUNCTION public.bump_version();
INSERT INTO public.guard_fixture(id) VALUES(1);
INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES('00000000-0000-4000-8000-000000000071','guard-fixture','synthetic','guard@example.invalid','00000000-0000-4000-8000-000000000072');
CREATE POLICY guard_executor_users ON public.users FOR ALL TO accounts_migration USING(true) WITH CHECK(true);
GRANT SELECT, UPDATE ON public.users TO accounts_migration;
GRANT USAGE ON SCHEMA public TO accounts_migration, ` + quoteBusiness(business) + `;
GRANT SELECT, UPDATE ON public.guard_fixture TO accounts_migration, ` + quoteBusiness(business)); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "sql", "compat", "migration-trigger-guards.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("guard accepted missing physical target identity")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = db.Exec(`SELECT set_config('accounts_migration_guard.system_identifier','wrong-target',false), set_config('accounts_migration_guard.database_oid',(SELECT oid::text FROM pg_database WHERE datname=current_database()),false)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("guard accepted incorrect physical target identity")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = db.Exec(`SELECT set_config('accounts_migration_guard.system_identifier',(SELECT system_identifier::text FROM pg_control_system()),false), set_config('accounts_migration_guard.database_oid',(SELECT oid::text FROM pg_database WHERE datname=current_database()),false)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = db.Exec(string(script)); err != nil {
			t.Fatalf("guard install or idempotent retry: %v", err)
		}
	}
	connect := func(user, password string) *sql.DB {
		t.Helper()
		roleCfg := dbCfg.Copy()
		roleCfg.User, roleCfg.Password = user, password
		c := stdlib.OpenDB(*roleCfg)
		if err := c.Ping(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	executor := connect("accounts_migration", "fixture-executor")
	app := connect(business, "fixture-business")
	app.SetMaxOpenConns(1)
	if _, err = app.Exec(`SET ROLE accounts_migration`); err == nil {
		t.Fatal("business login could assume migration role")
	}
	var version int64
	var updated time.Time
	if err = executor.QueryRow(`UPDATE public.guard_fixture SET version=70,updated_at='2020-01-01Z' WHERE id=1 RETURNING version,updated_at`).Scan(&version, &updated); err != nil || version != 70 || !updated.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("executor did not preserve source fields: %v", err)
	}
	var expectedUser string
	if err = executor.QueryRow(`SELECT (to_jsonb(u) || jsonb_build_object('version',70,'updated_at','2020-01-01Z'::timestamptz))::text FROM public.users u WHERE uuid='00000000-0000-4000-8000-000000000071'`).Scan(&expectedUser); err != nil {
		t.Fatal(err)
	}
	if err = executor.QueryRow(`UPDATE public.users SET version=70,updated_at='2020-01-01Z' WHERE uuid='00000000-0000-4000-8000-000000000071' RETURNING version,updated_at`).Scan(&version, &updated); err != nil || version != 70 || !updated.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("executor did not preserve native users source fields: %v", err)
	}
	var equal bool
	if err = executor.QueryRow(`SELECT to_jsonb(u)=$1::jsonb FROM public.users u WHERE uuid='00000000-0000-4000-8000-000000000071'`, expectedUser).Scan(&equal); err != nil || !equal {
		t.Fatalf("native users row differs after source-field update: %v", err)
	}
	if err = app.QueryRow(`UPDATE public.guard_fixture SET version=100,updated_at='2020-01-01Z' WHERE id=1 RETURNING version,updated_at`).Scan(&version, &updated); err != nil || version != 71 || updated.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("business trigger behavior changed: %v", err)
	}
	if _, err = admin.Exec(`GRANT accounts_migration TO ` + quoteBusiness(business)); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Exec(`SET ROLE accounts_migration`); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Exec(`UPDATE public.guard_fixture SET version=900 WHERE id=1`); err == nil {
		t.Fatal("trigger accepted an assumable migration identity")
	}
	if _, err = app.Exec(`RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("guard installed when business could assume migration role")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = admin.Exec(`REVOKE accounts_migration FROM ` + quoteBusiness(business)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ALTER TABLE public.guard_fixture OWNER TO accounts_migration`); err != nil {
		t.Fatal(err)
	}
	if _, err = executor.Exec(`UPDATE public.guard_fixture SET version=901 WHERE id=1`); err == nil {
		t.Fatal("trigger accepted a migration role that owns a business table")
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("guard installed when migration role owned a business table")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = db.Exec(`ALTER TABLE public.guard_fixture OWNER TO postgres`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ALTER FUNCTION public.bump_version() SET search_path='pg_catalog'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("guard replaced a function with unreviewed execution settings")
	}
	_, _ = db.Exec(`ROLLBACK`)
	if _, err = db.Exec(`ALTER FUNCTION public.bump_version() RESET search_path`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE OR REPLACE FUNCTION public.set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err == nil {
		t.Fatal("guard replaced an unrecognized trigger function")
	}
	_, _ = db.Exec(`ROLLBACK`)
}
