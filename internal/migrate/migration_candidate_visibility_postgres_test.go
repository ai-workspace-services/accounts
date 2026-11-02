package migrate

import (
	"context"
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/url"
	"os"
	"testing"
)

func TestMigrationCandidateVisibilityPostgres(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_RUNTIME_RLS_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable database required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.User.Username() != "postgres" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/account" {
		t.Fatal("requires disposable loopback database")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	_, err = admin.Exec(`CREATE ROLE accounts_migration LOGIN PASSWORD 'fixture-password' NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE TABLE public.migration_fixture(id integer PRIMARY KEY); ALTER TABLE public.migration_fixture ENABLE ROW LEVEL SECURITY; GRANT USAGE ON SCHEMA public TO accounts_migration; GRANT SELECT ON public.migration_fixture TO accounts_migration;`)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("accounts_migration", "fixture-password")
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check := func() error {
		tx, e := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		return migrationCandidateVisibility(context.Background(), tx, "migration_fixture")
	}
	if check() == nil {
		t.Fatal("missing policy accepted")
	}
	_, err = admin.Exec(`CREATE POLICY migration_candidate_access ON public.migration_fixture FOR ALL TO accounts_migration USING (true) WITH CHECK (true)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = check(); err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}
	_, err = admin.Exec(`CREATE POLICY deny_fixture ON public.migration_fixture AS RESTRICTIVE FOR SELECT TO PUBLIC USING (false)`)
	if err != nil {
		t.Fatal(err)
	}
	if check() == nil {
		t.Fatal("restrictive policy accepted")
	}
	_, err = admin.Exec(`DROP POLICY deny_fixture ON public.migration_fixture; REVOKE SELECT ON public.migration_fixture FROM accounts_migration`)
	if err != nil {
		t.Fatal(err)
	}
	if check() == nil {
		t.Fatal("missing SELECT accepted")
	}
	_, err = admin.Exec(`GRANT SELECT ON public.migration_fixture TO accounts_migration; CREATE ROLE migration_fixture_parent; GRANT migration_fixture_parent TO accounts_migration`)
	if err != nil {
		t.Fatal(err)
	}
	if check() == nil {
		t.Fatal("role membership accepted")
	}
	_, err = admin.Exec(`REVOKE migration_fixture_parent FROM accounts_migration; DROP POLICY migration_candidate_access ON public.migration_fixture; CREATE POLICY migration_candidate_access ON public.migration_fixture FOR ALL TO accounts_migration USING (true) WITH CHECK (false)`)
	if err != nil {
		t.Fatal(err)
	}
	if check() == nil {
		t.Fatal("incomplete write policy accepted")
	}
}
