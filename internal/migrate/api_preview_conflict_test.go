package migrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationUnionErrorDistinguishesUniqueViolation(t *testing.T) {
	var conflict *migrationUniqueConflict
	err := migrationUnionError("users", &pgconn.PgError{Code: "23505", Detail: "secret-value"})
	if !errors.As(err, &conflict) || conflict.table != "users" || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unique violation not safely classified: %v", err)
	}
	err = migrationUnionError("users", &pgconn.PgError{Code: "42501", Detail: "secret-value"})
	if errors.As(err, &conflict) || err.Error() != "target union insert unavailable" {
		t.Fatalf("non-unique failure was misclassified: %v", err)
	}
}

func TestPreviewConflictErrorDisclosure(t *testing.T) {
	for _, tc := range []struct {
		err       error
		want      string
		forbidden string
	}{
		{&migrationUniqueConflict{table: "users"}, `"error":"target_unique_conflict"`, `secret-value`},
		{&migrationUniqueConflict{table: "secret-value"}, `"error":"preview_conflicted_or_unavailable"`, `secret-value`},
		{errors.New("database detail secret-value"), `"error":"preview_conflicted_or_unavailable"`, `secret-value`},
	} {
		w := httptest.NewRecorder()
		migrationWritePreviewError(w, tc.err)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), tc.forbidden) {
			t.Fatalf("unsafe preview refusal: %d %q", w.Code, w.Body.String())
		}
	}
}

func TestMigrationHTTPConflictDetailWhitelist(t *testing.T) {
	for _, tc := range []struct{ body, want, forbidden string }{
		{`{"error":"target_unique_conflict","table":"users"}`, "target_unique_conflict table=users", "secret-value"},
		{`{"error":"target_unique_conflict","table":"secret-value"}`, "HTTP 409", "secret-value"},
		{`{"error":"secret-value","table":"users"}`, "HTTP 409", "secret-value"},
		{strings.Repeat("x", 4097), "HTTP 409", strings.Repeat("x", 20)},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(tc.body))
		}))
		err := migrationHTTP(context.Background(), server.Client(), "POST", server.URL+MigrationAPIBase+"/plans/preview", "synthetic-token", nil, nil)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.forbidden) || strings.Contains(err.Error(), "synthetic-token") {
			t.Fatalf("unsafe HTTP error: %v", err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"target_unique_conflict","table":"users"}`))
	}))
	defer server.Close()
	err := migrationHTTP(context.Background(), server.Client(), "POST", server.URL+MigrationAPIBase+"/snapshots", "synthetic-token", nil, nil)
	if err == nil || strings.Contains(err.Error(), "target_unique_conflict") {
		t.Fatalf("source response was misclassified as target conflict: %v", err)
	}
}
