package store

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestACLPolicyMigrationBuildHistoryAndSafety(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "sql", "migrations", "2026082804_overlay_acl_compiler.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"overlay_policy_revisions", "overlay_policy_builds", "overlay_policy_one_active_per_network_uk", "default_action'='deny'", "overlay_policy_revision_secret_ck"} {
		if !strings.Contains(text, want) {
			t.Errorf("migration missing %q", want)
		}
	}
}
func TestPostgresGetActiveOverlayPolicy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	columns := []string{"network_id", "revision", "owner_user_uuid", "name", "source", "artifact", "artifact_sha256", "compiler_version", "warnings", "status", "generation", "created_at", "validated_at", "activated_at"}
	mock.ExpectQuery("FROM public.overlay_policy_revisions WHERE network_id=\\$1 AND status='active'").WithArgs("net").WillReturnRows(sqlmock.NewRows(columns).AddRow("net", uint64(1), "owner", "p", []byte(`{}`), []byte(`{}`), strings.Repeat("a", 64), "v", []byte(`[]`), "active", uint64(1), now, now, now))
	p, err := st.GetActiveOverlayPolicy(context.Background(), "net")
	if err != nil || p.Generation != 1 {
		t.Fatalf("p=%#v err=%v", p, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
