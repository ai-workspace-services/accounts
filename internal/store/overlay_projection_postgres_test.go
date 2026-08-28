package store

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPostgresOverlayProjectionSaveUsesTransactionLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	record := &OverlaySignedConfigRecord{
		UserID: "11111111-1111-1111-1111-111111111111", DeviceID: "dev-1", ConfigID: "cfg-1",
		NetworkID: "net-1", Generation: 1, SourceRevision: "source-1", SigningKeyID: "key-1",
		SignedPayload: []byte(`{"proxy_core":"xray"}`), IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1::text || chr(31) || $2, 0))")).
		WithArgs(record.UserID, record.DeviceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT generation, source_revision").WithArgs(record.UserID, record.DeviceID).
		WillReturnRows(sqlmock.NewRows([]string{"generation", "source_revision"}))
	mock.ExpectExec("INSERT INTO public.overlay_signed_configs").
		WithArgs(record.UserID, record.DeviceID, record.ConfigID, record.NetworkID, record.Generation,
			record.SourceRevision, record.SigningKeyID, string(record.SignedPayload), record.IssuedAt, record.ExpiresAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := st.SaveOverlaySignedConfig(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresOverlaySigningKeyMaxExpiryUsesPersistedHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	want := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	mock.ExpectQuery("(?s)SELECT COALESCE\\(MAX\\(expires_at\\).*overlay_gateway_snapshots").WithArgs("key-old").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(want))
	got, err := st.GetOverlaySigningKeyMaxExpiresAt(context.Background(), "key-old")
	if err != nil || !got.Equal(want) {
		t.Fatalf("max expiry=%v err=%v, want %v", got, err, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayProjectionMigrationLocksSafetyConstraints(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "sql", "migrations", "2026082801_overlay_signed_config_projection.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := string(raw)
	for _, required := range []string{
		"PRIMARY KEY (user_uuid, device_id, generation)",
		"overlay_signed_configs_config_uk",
		"signed_payload->>'proxy_core' = 'xray'",
		"private_key|refresh_token|vault_token",
		"FOREIGN KEY (user_uuid, device_id, generation, config_id)",
	} {
		if !strings.Contains(sqlText, required) {
			t.Errorf("migration missing %q", required)
		}
	}
}
