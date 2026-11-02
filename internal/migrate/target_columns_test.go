package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestTargetColumnContractIgnoresOnlyPhysicalOrder(t *testing.T) {
	want := []businessColumn{{Name: "id", Type: "uuid"}, {Name: "email", Type: "text", Nullable: true}, {Name: "verified", Type: "boolean", Nullable: true, Generated: true}}
	actual := []ColumnDefinition{{Name: "verified", Type: "boolean", Nullable: true, Generated: true}, {Name: "id", Type: "uuid"}, {Name: "email", Type: "text", Nullable: true}}
	if err := validateBusinessTargetColumns("users", actual, want); err != nil {
		t.Fatalf("equivalent reordered columns rejected: %v", err)
	}
	cases := map[string]func([]ColumnDefinition) []ColumnDefinition{
		"missing":   func(c []ColumnDefinition) []ColumnDefinition { return c[:2] },
		"extra":     func(c []ColumnDefinition) []ColumnDefinition { return append(c, ColumnDefinition{Name: "extra"}) },
		"duplicate": func(c []ColumnDefinition) []ColumnDefinition { c[0].Name = "id"; return c },
		"type":      func(c []ColumnDefinition) []ColumnDefinition { c[1].Type = "text"; return c },
		"nullable":  func(c []ColumnDefinition) []ColumnDefinition { c[2].Nullable = false; return c },
		"generated": func(c []ColumnDefinition) []ColumnDefinition { c[0].Generated = false; return c },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := mutate(append([]ColumnDefinition(nil), actual...))
			if err := validateBusinessTargetColumns("users", input, want); err == nil {
				t.Fatal("incompatible columns accepted")
			}
		})
	}
}

// This checks only an independently prepared, schema-only loopback fixture.
// It does not assert backup recovery, row compatibility, or writer safety.
func TestExistingSchemaMigrationCatalogPostgres(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_SCHEMA_REVIEW_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated schema review fixture not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.User.Username() != "postgres" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || !strings.HasPrefix(u.Path, "/v6_schema_review_") {
		t.Fatal("requires isolated loopback schema review database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tables, order, err := migrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range order {
		if err = validateBusinessTargetCatalog(ctx, tx, name, tables[name], false); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err = migrationSchema(ctx, tx, order); err == nil {
		t.Fatal("unversioned fixture accepted without opt-in")
	}
	optedIn := context.WithValue(ctx, migrationUnversionedTargetKey{}, true)
	if fingerprint, e := migrationSchema(optedIn, tx, order); e != nil || !migrationSHA.MatchString(fingerprint) {
		t.Fatal("unversioned fixture fingerprint unavailable", e)
	}
	if mode, e := migrationTargetSchemaMode(optedIn, tx); e != nil || mode != migrationUnversionedTargetMode {
		t.Fatal("unversioned fixture mode not reported", e)
	}
}

func TestTargetColumnOrderPostgres(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_API_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL test database not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.User.Username() != "postgres" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/postgres" {
		t.Fatal("requires disposable loopback postgres test database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE public.migration_column_order_fixture (email text, id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	table := businessTable{Columns: []businessColumn{{Name: "id", Type: "uuid"}, {Name: "email", Type: "text", Nullable: true}}, PrimaryKey: []string{"id"}}
	if err = validateBusinessTargetCatalog(ctx, tx, "migration_column_order_fixture", table, false); err != nil {
		t.Fatalf("reordered PostgreSQL table rejected: %v", err)
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE public.migration_column_order_fixture ALTER COLUMN email SET NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if err = validateBusinessTargetCatalog(ctx, tx, "migration_column_order_fixture", table, false); err == nil {
		t.Fatal("incompatible nullability accepted")
	}
}
