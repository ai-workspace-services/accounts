package migrate

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPreviewUniqueViolationPostgres(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_API_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "postgres" || cfg.User != "postgres" || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") {
		t.Fatal("dedicated loopback PostgreSQL required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TEMP TABLE conflict_fixture(username text UNIQUE); INSERT INTO conflict_fixture VALUES ('fixture');`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO conflict_fixture VALUES ('fixture')`)
	if err == nil {
		t.Fatal("unique violation missing")
	}
	w := httptest.NewRecorder()
	migrationWritePreviewError(w, migrationUnionError("users", err))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error":"target_unique_conflict"`) || !strings.Contains(w.Body.String(), `"table":"users"`) || strings.Contains(w.Body.String(), "fixture") {
		t.Fatalf("unexpected safe conflict response: %s", w.Body.String())
	}
}
