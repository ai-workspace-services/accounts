package store

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGatewayMigrationLocksHashShadowAndNetworkUniqueness(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "sql", "migrations", "2026082803_overlay_gateway_projection.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{"token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash)=32)", "mode='shadow'", "proxy_core='xray'", "applied_generation=0", "runtime_applied=FALSE", "overlay_gateway_snapshots_source_uk", "overlay_gateway_snapshots_secret_ck", "vless-tls-xudp", "overlay_devices_network_device_id_uk", "overlay_devices_network_public_key_uk", "idx_overlay_devices_network_address", "body_sha256"} {
		if !strings.Contains(text, required) {
			t.Errorf("migration missing %q", required)
		}
	}
	for _, forbidden := range []string{"raw_token", "token_secret", "credential_secret"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("migration persists forbidden %q", forbidden)
		}
	}
}

func TestPostgresNodeCredentialCreateIsHashOnlyAndAudited(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	digest := sha256.Sum256([]byte("xgn_secret"))
	credential := &OverlayNodeCredential{ID: "cred_test_01", NodeID: "gw_test_01", TokenHash: digest[:], ExpiresAt: now.Add(time.Hour)}
	audit := &AuditLog{Action: AuditActionOverlayNodeCredentialCreate, Details: map[string]any{"credential_id": credential.ID}}
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO public.overlay_node_credentials").WithArgs(credential.ID, credential.NodeID, credential.TokenHash, credential.ExpiresAt).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectQuery("INSERT INTO public.audit_logs").WithArgs(sqlmock.AnyArg(), audit.Action, nil, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	if err := st.CreateOverlayNodeCredential(context.Background(), credential, audit); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresGatewaySnapshotGenerationUsesTransactionLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	record := &OverlayGatewaySnapshotRecord{NodeID: "gw_test_01", SnapshotID: "snap_test_01", Generation: 1, ExpectedPreviousGeneration: 0, SourceRevision: "sha256:source", SigningKeyID: "key_test_01", SignedPayload: []byte(`{"proxy_core":"xray"}`), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs(record.NodeID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM public.overlay_gateway_snapshots WHERE node_id=$1 ORDER BY generation DESC LIMIT 1 FOR UPDATE")).WithArgs(record.NodeID).WillReturnRows(sqlmock.NewRows([]string{"node_id", "snapshot_id", "generation", "expected_previous_generation", "source_revision", "signing_key_id", "signed_payload", "issued_at", "expires_at", "created_at"}))
	mock.ExpectQuery("INSERT INTO public.overlay_gateway_snapshots").WithArgs(record.NodeID, record.SnapshotID, record.Generation, record.ExpectedPreviousGeneration, record.SourceRevision, record.SigningKeyID, string(record.SignedPayload), record.IssuedAt, record.ExpiresAt).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	if err := st.SaveOverlayGatewaySnapshot(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStaticImportClaimsWireGuardKeyInSameTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	input := &OverlayStaticImport{
		IdempotencyKey: "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BodySHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		OwnerUserID:    "11111111-1111-4111-8111-111111111111",
		NetworkID:      "net",
		SourceKind:     "ansible-group-vars",
		SourceVariable: "clients",
		BaselineSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Devices:        []OverlayProjectionDevice{{Device: OverlayDevice{ID: "imported", Name: "imported", Platform: "legacy-import", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", WireGuardAddress: "10.77.0.10/32"}}},
	}
	audit := &AuditLog{Action: AuditActionOverlayStaticImport}
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM public.overlay_static_import_receipts WHERE idempotency_key=\\$1").WithArgs(input.IdempotencyKey).WillReturnRows(sqlmock.NewRows([]string{"import_id", "idempotency_key", "body_sha256", "owner_user_uuid", "network_id", "baseline_sha256", "device_count", "created_at"}))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM public.users").WithArgs(input.OwnerUserID).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("SELECT user_uuid::text,network_id,wireguard_public_key,wireguard_address,status FROM public.overlay_devices WHERE id=\\$1 FOR UPDATE").WithArgs(input.Devices[0].Device.ID).WillReturnRows(sqlmock.NewRows([]string{"user_uuid", "network_id", "wireguard_public_key", "wireguard_address", "status"}))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM public.overlay_devices").WithArgs(input.NetworkID, input.Devices[0].Device.ID, input.Devices[0].Device.WireGuardPublicKey, input.Devices[0].Device.WireGuardAddress).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("INSERT INTO public.overlay_device_key_history").WithArgs(input.NetworkID, input.Devices[0].Device.WireGuardPublicKey, input.OwnerUserID, input.Devices[0].Device.ID, uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"wireguard_public_key"}).AddRow(input.Devices[0].Device.WireGuardPublicKey))
	mock.ExpectExec("INSERT INTO public.overlay_devices").WithArgs(input.Devices[0].Device.ID, input.OwnerUserID, input.NetworkID, input.Devices[0].Device.Name, input.Devices[0].Device.Platform, "", input.Devices[0].Device.WireGuardPublicKey, input.Devices[0].Device.WireGuardAddress).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO public.overlay_device_projection_metadata").WithArgs(input.OwnerUserID, input.Devices[0].Device.ID, "null", "null", input.SourceKind, input.SourceVariable, input.BaselineSHA256).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("INSERT INTO public.overlay_static_import_receipts").WithArgs("import_"+input.BodySHA256[:24], input.IdempotencyKey, input.BodySHA256, input.OwnerUserID, input.NetworkID, input.SourceKind, input.SourceVariable, input.BaselineSHA256, 1).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectQuery("INSERT INTO public.audit_logs").WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	receipt, duplicate, err := st.ImportOverlayStaticClients(context.Background(), input, audit)
	if err != nil || duplicate || receipt == nil || receipt.DeviceCount != 1 {
		t.Fatalf("receipt=%+v duplicate=%v err=%v", receipt, duplicate, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
