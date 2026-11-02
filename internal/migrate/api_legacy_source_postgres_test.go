package migrate

import (
	schema "account/sql"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Optional qualification against the immutable predecessor's published SQL.
// No live DSN or live data is read; databases are random and loopback-only.
func TestMigrationAPIPredecessorArtifact(t *testing.T) {
	path := os.Getenv("MIGRATECTL_API_PREDECESSOR_SQL")
	if path == "" {
		t.Skip("reviewed predecessor SQL artifact not supplied")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(body)) != "f45db2423f11d9dcc7f3cc58c72b52228a27d7b6ccfc717faf2bbb56fa0498b1" {
		t.Fatal("predecessor artifact checksum differs")
	}
	cfg, err := pgx.ParseConfig(os.Getenv("MIGRATECTL_API_TEST_DSN"))
	if err != nil || cfg.Database != "postgres" || cfg.Host != "127.0.0.1" {
		t.Fatal("dedicated loopback fixture required")
	}
	admin := stdlib.OpenDB(*cfg)
	defer admin.Close()
	name := "migration_api_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE DATABASE ` + quoteBusiness(name)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP DATABASE ` + quoteBusiness(name) + ` WITH (FORCE)`)
	config := cfg.Copy()
	config.Database = name
	db := stdlib.OpenDB(*config)
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	exec(string(body))
	exec(`CREATE TABLE public.schema_migrations(version bigint PRIMARY KEY,dirty boolean NOT NULL); INSERT INTO public.schema_migrations VALUES(2026102002,false)`)
	if _, err = admin.Exec(`DO $$ BEGIN IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='readonly_release') THEN CREATE ROLE readonly_release LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; END IF; END $$; ALTER ROLE readonly_release PASSWORD 'isolated-readonly-fixture'`); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES ('00000000-0000-0000-0000-000000000101','legacy-fixture','fixture-password-hash','legacy@example.invalid','00000000-0000-0000-0000-000000000201');
 INSERT INTO public.billing_plans(plan_id,display_name) VALUES('legacy-fixture-plan','fixture');
 INSERT INTO public.subscriptions(uuid,user_uuid,provider,external_id,plan_id) VALUES('00000000-0000-0000-0000-000000000301','00000000-0000-0000-0000-000000000101','local','fixture-sub','legacy-fixture-plan');
 INSERT INTO public.subscription_records(user_uuid,monthly_quota_bytes) VALUES('00000000-0000-0000-0000-000000000101',9007199254740993)`)
	exec(`GRANT USAGE ON SCHEMA public TO readonly_release; GRANT SELECT ON ALL TABLES IN SCHEMA public TO readonly_release; GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO readonly_release`)
	for _, table := range migrationAPITables {
		var rls bool
		e := db.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid=to_regclass($1)`, "public."+table).Scan(&rls)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		if rls {
			exec(`CREATE POLICY release_initialization_readonly ON public.` + quoteBusiness(table) + ` FOR SELECT TO readonly_release USING(true)`)
		}
	}
	config.User = "readonly_release"
	config.Password = "isolated-readonly-fixture"
	reader := stdlib.OpenDB(*config)
	defer reader.Close()
	snapshot, err := CaptureMigrationSnapshot(context.Background(), reader, uuid.NewString(), time.Hour, MigrationAuthorization{SourceAudience: "source-fixture", TargetAudience: "target-fixture", JobID: uuid.NewString(), SubjectSHA256: migrationDigest("operator")})
	if err != nil {
		t.Fatal(err)
	}
	targetName := "migration_api_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE DATABASE ` + quoteBusiness(targetName)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP DATABASE ` + quoteBusiness(targetName) + ` WITH (FORCE)`)
	targetConfig := cfg.Copy()
	targetConfig.Database = targetName
	target := stdlib.OpenDB(*targetConfig)
	defer target.Close()
	native, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Exec(string(native)); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Exec(fmt.Sprintf(`CREATE TABLE public.schema_migrations(version bigint PRIMARY KEY,dirty boolean NOT NULL); INSERT INTO public.schema_migrations VALUES(%d,false)`, FullBusinessVersion)); err != nil {
		t.Fatal(err)
	}
	if err = InitializeMigrationReceipts(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewMigration(context.Background(), target, snapshot, uuid.NewString(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ConflictRows != 0 || plan.InsertRows < 4 {
		t.Fatal("legacy business plan incomplete")
	}
	if _, err = ApplyMigration(context.Background(), target, snapshot, plan, plan.PlanSHA256, "operator"); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = target.QueryRow(`SELECT EXISTS(SELECT 1 FROM public.subscription_records WHERE monthly_quota_bytes=9007199254740993) AND EXISTS(SELECT 1 FROM public.users WHERE uuid='00000000-0000-0000-0000-000000000101' AND password='fixture-password-hash')`).Scan(&exact); err != nil || !exact {
		t.Fatal("legacy identity or quota changed")
	}
}
