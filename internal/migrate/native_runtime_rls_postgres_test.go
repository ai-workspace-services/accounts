package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Only a disposable loopback database may run these DDL tests.
func TestRuntimePolicyVisibilityPostgres(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_RUNTIME_RLS_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable RLS fixture not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/account" || u.User.Username() != "postgres" {
		t.Fatal("requires disposable loopback account database")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(`CREATE ROLE rls_runtime_ci LOGIN PASSWORD 'synthetic-runtime' NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE TABLE public.rls_runtime_fixture (id integer PRIMARY KEY);
ALTER TABLE public.rls_runtime_fixture ENABLE ROW LEVEL SECURITY;
GRANT CONNECT ON DATABASE account TO rls_runtime_ci;
GRANT USAGE ON SCHEMA public TO rls_runtime_ci;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.rls_runtime_fixture TO rls_runtime_ci`); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("rls_runtime_ci", "synthetic-runtime")
	runtime, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx := context.Background()
	table := businessTable{Columns: []businessColumn{{Name: "id", Type: "integer", Nullable: false}}, PrimaryKey: []string{"id"}}
	check := func(runtimeMode bool) error {
		t.Helper()
		tx, err := runtime.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return validateBusinessTargetCatalog(ctx, tx, "rls_runtime_fixture", table, runtimeMode)
	}
	if check(true) == nil {
		t.Fatal("runtime accepted missing RLS policy")
	}
	if _, err = admin.Exec(`CREATE POLICY accounts_runtime_access ON public.rls_runtime_fixture FOR ALL TO rls_runtime_ci USING (true) WITH CHECK (true)`); err != nil {
		t.Fatal(err)
	}
	if err = check(true); err != nil {
		t.Fatalf("runtime rejected scoped policy: %v", err)
	}
	if check(false) == nil {
		t.Fatal("migration accepted runtime-only policy")
	}
	if _, err = admin.Exec(`CREATE POLICY runtime_restrict ON public.rls_runtime_fixture AS RESTRICTIVE FOR SELECT TO rls_runtime_ci USING (false)`); err != nil {
		t.Fatal(err)
	}
	if check(true) == nil {
		t.Fatal("runtime accepted restrictive RLS policy")
	}
	if _, err = admin.Exec(`DROP POLICY runtime_restrict ON public.rls_runtime_fixture; DROP POLICY accounts_runtime_access ON public.rls_runtime_fixture; CREATE POLICY accounts_runtime_access ON public.rls_runtime_fixture FOR ALL TO rls_runtime_ci USING (true) WITH CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	if check(true) == nil {
		t.Fatal("runtime accepted a denied write policy")
	}
}
