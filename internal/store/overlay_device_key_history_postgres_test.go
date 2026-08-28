package store

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPostgresUpsertOverlayDeviceClaimsKeyInRegistrationTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	columns := []string{"id", "user_uuid", "network_id", "name", "platform", "hostname", "wireguard_public_key", "wireguard_address", "created_at", "updated_at", "last_seen_at", "status", "state_version", "key_version", "revoked_at", "revoked_reason"}
	device := &OverlayDevice{ID: "dev-register", UserID: "11111111-1111-4111-8111-111111111111", NetworkID: "net", Name: "device", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", WireGuardAddress: "10.77.0.2/32"}
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs(device.NetworkID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason FROM public.overlay_devices WHERE user_uuid=$1 AND id=$2 FOR UPDATE")).WithArgs(device.UserID, device.ID).WillReturnRows(sqlmock.NewRows(columns))
	mock.ExpectQuery("INSERT INTO public.overlay_device_key_history").WithArgs(device.NetworkID, device.WireGuardPublicKey, device.UserID, device.ID, uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"wireguard_public_key"}).AddRow(device.WireGuardPublicKey))
	mock.ExpectQuery("INSERT INTO public.overlay_devices").WithArgs(device.ID, device.UserID, device.NetworkID, device.Name, device.Platform, "", device.WireGuardPublicKey, device.WireGuardAddress, nil).WillReturnRows(sqlmock.NewRows(columns).AddRow(device.ID, device.UserID, device.NetworkID, device.Name, device.Platform, "", device.WireGuardPublicKey, device.WireGuardAddress, now, now, nil, OverlayDeviceActive, uint64(1), uint64(1), nil, ""))
	mock.ExpectQuery("INSERT INTO public.overlay_device_events").WithArgs(device.UserID, device.NetworkID, device.ID, "registered", OverlayDeviceActive, uint64(1), uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"sequence", "created_at"}).AddRow(1, now))
	mock.ExpectCommit()
	if err := st.UpsertOverlayDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresUpsertOverlayDeviceRejectsHistoricalKeyClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	columns := []string{"id", "user_uuid", "network_id", "name", "platform", "hostname", "wireguard_public_key", "wireguard_address", "created_at", "updated_at", "last_seen_at", "status", "state_version", "key_version", "revoked_at", "revoked_reason"}
	device := &OverlayDevice{ID: "replacement", UserID: "11111111-1111-4111-8111-111111111111", NetworkID: "net", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs(device.NetworkID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM public.overlay_devices WHERE user_uuid=\\$1 AND id=\\$2 FOR UPDATE").WithArgs(device.UserID, device.ID).WillReturnRows(sqlmock.NewRows(columns))
	mock.ExpectQuery("INSERT INTO public.overlay_device_key_history").WithArgs(device.NetworkID, device.WireGuardPublicKey, device.UserID, device.ID, uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"wireguard_public_key"}))
	mock.ExpectRollback()
	if err := st.UpsertOverlayDevice(context.Background(), device); !errors.Is(err, ErrOverlayDeviceKeyConflict) {
		t.Fatalf("historical key claim err=%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
