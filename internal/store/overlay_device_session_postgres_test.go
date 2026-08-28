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

func credentialSQLRow(now time.Time, verifier []byte) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"credential_id", "verifier_sha256", "user_uuid", "network_id", "device_id", "status", "scope", "replaces_credential_id", "replaced_by_credential_id", "rotation_request_sha256", "issued_at", "expires_at", "revoked_at", "created_at", "device_status", "device_network", "user_active"}).
		AddRow("xdcid_0123456789abcdef0123456789abcdef", verifier, "11111111-1111-1111-1111-111111111111", "network-1", "device-1", OverlayDeviceCredentialActive, `["overlay:session:mint","overlay:credential:rotate","overlay:device:revoke"]`, nil, nil, nil, now, now.Add(30*24*time.Hour), nil, now, OverlayDeviceActive, "network-1", true)
}

func credentialBaseSQLRow(now time.Time, verifier []byte, status, replacedBy string) *sqlmock.Rows {
	var replacedByValue any
	if replacedBy != "" {
		replacedByValue = replacedBy
	}
	return sqlmock.NewRows([]string{"credential_id", "verifier_sha256", "user_uuid", "network_id", "device_id", "status", "scope", "replaces_credential_id", "replaced_by_credential_id", "rotation_request_sha256", "issued_at", "expires_at", "revoked_at", "created_at"}).
		AddRow("xdcid_0123456789abcdef0123456789abcdef", verifier, "11111111-1111-1111-1111-111111111111", "network-1", "device-1", status, `["overlay:session:mint","overlay:credential:rotate","overlay:device:revoke"]`, nil, replacedByValue, nil, now, now.Add(30*24*time.Hour), nil, now)
}

func TestPostgresDeviceCredentialAuthenticationUsesQualifiedBindingAndVerifier(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	verifier := sha256.Sum256([]byte("credential"))
	mock.ExpectQuery(regexp.QuoteMeta("FROM public.overlay_device_credentials c JOIN public.overlay_devices d")).WithArgs("xdcid_0123456789abcdef0123456789abcdef").WillReturnRows(credentialSQLRow(now, verifier[:]))
	credential, err := st.AuthenticateOverlayDeviceCredential(context.Background(), "xdcid_0123456789abcdef0123456789abcdef", verifier[:], now)
	if err != nil || credential.DeviceID != "device-1" {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresDeviceSessionMintIsBoundAndAuditedAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	tokenHash := sha256.Sum256([]byte("xenr_session"))
	session := &OverlayEnrollmentSession{ID: "session-1", TokenHash: tokenHash[:], Scopes: append([]string(nil), overlayDeviceSessionScopes...), ExpiresAt: now.Add(10 * time.Minute)}
	audit := &AuditLog{Action: AuditActionOverlayDeviceSessionMint, ActorUUID: "11111111-1111-1111-1111-111111111111", Details: map[string]any{}}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.user_uuid::text,c.network_id,c.device_id,c.status,c.expires_at,d.status,u.active")).WithArgs("xdcid_0123456789abcdef0123456789abcdef").WillReturnRows(sqlmock.NewRows([]string{"user_uuid", "network_id", "device_id", "status", "expires_at", "device_status", "active"}).AddRow(audit.ActorUUID, "network-1", "device-1", OverlayDeviceCredentialActive, now.Add(30*24*time.Hour), OverlayDeviceActive, true))
	mock.ExpectExec("INSERT INTO public.overlay_device_sessions").WithArgs(session.ID, session.TokenHash, "xdcid_0123456789abcdef0123456789abcdef", audit.ActorUUID, "network-1", "device-1", `["overlay:config:read","overlay:config:ack"]`, session.ExpiresAt, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("INSERT INTO public.audit_logs").WithArgs(sqlmock.AnyArg(), audit.Action, audit.ActorUUID, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	if err = st.MintOverlayDeviceSession(context.Background(), "xdcid_0123456789abcdef0123456789abcdef", session, now, audit); err != nil {
		t.Fatal(err)
	}
	if session.UserID != audit.ActorUUID || session.NetworkID != "network-1" || session.DeviceID != "device-1" {
		t.Fatalf("session binding not persisted in response: %#v", session)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresDeviceCredentialRotationIsOneTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	currentVerifier := sha256.Sum256([]byte("current"))
	successorVerifier := sha256.Sum256([]byte("successor"))
	successor := &OverlayDeviceCredential{ID: "xdcid_fedcba9876543210fedcba9876543210", Verifier: successorVerifier[:], Scopes: append([]string(nil), overlayDeviceCredentialScopes...), ExpiresAt: now.Add(30 * 24 * time.Hour)}
	requestHash := strings.Repeat("a", 64)
	audit := &AuditLog{Action: AuditActionOverlayDeviceCredentialRotate, ActorUUID: "11111111-1111-1111-1111-111111111111", Details: map[string]any{}}
	mock.ExpectBegin()
	mock.ExpectQuery("FROM public.overlay_device_credentials c WHERE c.credential_id=\\$1 FOR UPDATE").WithArgs("xdcid_0123456789abcdef0123456789abcdef").WillReturnRows(credentialBaseSQLRow(now, currentVerifier[:], OverlayDeviceCredentialActive, ""))
	mock.ExpectQuery("SELECT d.status,u.active").WithArgs(audit.ActorUUID, "network-1", "device-1").WillReturnRows(sqlmock.NewRows([]string{"status", "active"}).AddRow(OverlayDeviceActive, true))
	mock.ExpectExec("UPDATE public.overlay_device_credentials SET status='replaced'").WithArgs("xdcid_0123456789abcdef0123456789abcdef").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO public.overlay_device_credentials").WithArgs(successor.ID, successor.Verifier, audit.ActorUUID, "network-1", "device-1", `["overlay:session:mint","overlay:credential:rotate","overlay:device:revoke"]`, "xdcid_0123456789abcdef0123456789abcdef", requestHash, now, successor.ExpiresAt).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectExec("UPDATE public.overlay_device_credentials SET replaced_by_credential_id").WithArgs("xdcid_0123456789abcdef0123456789abcdef", successor.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO public.audit_logs").WithArgs(sqlmock.AnyArg(), audit.Action, audit.ActorUUID, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	stored, duplicate, err := st.RotateOverlayDeviceCredential(context.Background(), "xdcid_0123456789abcdef0123456789abcdef", successor, requestHash, now, audit)
	if err != nil || duplicate || stored.ReplacesCredentialID != "xdcid_0123456789abcdef0123456789abcdef" {
		t.Fatalf("stored=%#v duplicate=%v err=%v", stored, duplicate, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresHistoricalVerifierReplaysExactTerminalReceipt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	verifier := sha256.Sum256([]byte("historical"))
	requestHash := strings.Repeat("b", 64)
	nonce := "11111111-1111-4111-8111-111111111111"
	devicePayload := `{"id":"device-1","user_id":"11111111-1111-1111-1111-111111111111","network_id":"network-1","status":"revoked"}`
	mock.ExpectBegin()
	mock.ExpectQuery("FROM public.overlay_device_credentials c WHERE c.credential_id=\\$1 FOR UPDATE").WithArgs("xdcid_0123456789abcdef0123456789abcdef").WillReturnRows(credentialBaseSQLRow(now, verifier[:], OverlayDeviceCredentialRevoked, ""))
	mock.ExpectQuery("SELECT credential_id,request_sha256,client_nonce::text,device_payload,created_at").WithArgs("11111111-1111-1111-1111-111111111111", "network-1", "device-1").WillReturnRows(sqlmock.NewRows([]string{"credential_id", "request_sha256", "client_nonce", "device_payload", "created_at"}).AddRow("xdcid_0123456789abcdef0123456789abcdef", requestHash, nonce, []byte(devicePayload), now))
	mock.ExpectCommit()
	receipt, duplicate, err := st.RevokeOverlayDeviceWithCredential(context.Background(), "xdcid_0123456789abcdef0123456789abcdef", verifier[:], requestHash, nonce, now.Add(time.Minute), &AuditLog{Action: AuditActionOverlayDeviceRevoke})
	if err != nil || !duplicate || receipt.Device.Status != OverlayDeviceRevoked {
		t.Fatalf("receipt=%#v duplicate=%v err=%v", receipt, duplicate, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceCredentialMigrationAndFreshSchemaAreHashOnly(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(filename), "..", "..")
	for _, relative := range []string{"sql/migrations/2026082807_overlay_device_credentials.up.sql", "sql/schema.sql", "sql/20260601_overlay_control_plane.sql"} {
		raw, err := os.ReadFile(filepath.Join(repo, relative))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, required := range []string{"overlay_device_credentials", "verifier_sha256", "overlay_device_sessions", "overlay_device_revoke_receipts", "31 days", "15 minutes"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s missing %q", relative, required)
			}
		}
		for _, forbidden := range []string{"raw_credential", "credential_secret", "device_refresh_token"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s persists forbidden field %q", relative, forbidden)
			}
		}
	}
	down, err := os.ReadFile(filepath.Join(repo, "sql/migrations/2026082807_overlay_device_credentials.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(down)
	if strings.Index(text, "overlay_device_revoke_receipts") > strings.Index(text, "overlay_device_credentials") || !strings.Contains(text, "Destructive") {
		t.Fatalf("unsafe rollback ordering/warning: %s", text)
	}
}
