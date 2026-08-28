package store

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDeviceLifecycleMigrationAndFreshSchemaContainSafetyState(t *testing.T) {
	up, err := os.ReadFile(filepath.Join("..", "..", "sql", "migrations", "2026082805_overlay_device_lifecycle.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "sql", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"overlay_device_events", "gateway_mode", "overlay_gateway_apply_attempts", "overlay_policy_reconcile_pending", "artifact_canonical BYTEA"} {
		if !strings.Contains(string(up), want) || !strings.Contains(string(schema), want) {
			t.Errorf("migration/fresh schema missing %q", want)
		}
	}
	if strings.Contains(string(up), "artifact::text") || strings.Contains(string(up), "artifact_canonical =") {
		t.Fatal("migration fabricated canonical policy bytes from JSONB")
	}
}

func TestPostgresRotateOverlayDeviceKeyIsTransactionalAndCanonical(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	columns := []string{"id", "user_uuid", "network_id", "name", "platform", "hostname", "wireguard_public_key", "wireguard_address", "created_at", "updated_at", "last_seen_at", "status", "state_version", "key_version", "revoked_at", "revoked_reason"}
	oldKey, newKey := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason FROM public.overlay_devices WHERE user_uuid=$1 AND network_id=$2 AND id=$3 FOR UPDATE")).WithArgs("11111111-1111-4111-8111-111111111111", "net", "dev-a").WillReturnRows(sqlmock.NewRows(columns).AddRow("dev-a", "11111111-1111-4111-8111-111111111111", "net", "device", "linux", "host", oldKey, "10.0.0.2/32", now, now, nil, "active", uint64(1), uint64(1), nil, ""))
	mock.ExpectQuery("SELECT EXISTS").WithArgs("net", "dev-a", newKey).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("UPDATE public.overlay_devices SET wireguard_public_key").WithArgs("11111111-1111-4111-8111-111111111111", "net", "dev-a", newKey).WillReturnRows(sqlmock.NewRows(columns).AddRow("dev-a", "11111111-1111-4111-8111-111111111111", "net", "device", "linux", "host", newKey, "10.0.0.2/32", now, now, nil, "active", uint64(2), uint64(2), nil, ""))
	mock.ExpectQuery("INSERT INTO public.overlay_device_events").WithArgs("11111111-1111-4111-8111-111111111111", "net", "dev-a", "key_rotated", "active", uint64(2), uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"sequence", "created_at"}).AddRow(2, now))
	mock.ExpectQuery("INSERT INTO public.audit_logs").WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	device, duplicate, err := st.RotateOverlayDeviceKey(context.Background(), "11111111-1111-4111-8111-111111111111", "net", "dev-a", newKey, 1, &AuditLog{Action: AuditActionOverlayDeviceKeyRotate})
	if err != nil || duplicate || device.WireGuardPublicKey != newKey || device.KeyVersion != 2 {
		t.Fatalf("device=%+v duplicate=%v err=%v", device, duplicate, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
