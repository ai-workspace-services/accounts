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

func TestPostgresCreateJoinTokenStoresHashAndAuditAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	digest := sha256.Sum256([]byte("xjt_secret"))
	token := &OverlayJoinToken{ID: "join-1", TokenHash: digest[:], UserID: "11111111-1111-1111-1111-111111111111", NetworkID: "net", RemainingUses: 1, ExpiresAt: now.Add(time.Hour)}
	audit := &AuditLog{Action: AuditActionOverlayJoinCreate, ActorUUID: token.UserID, Details: map[string]any{"join_token_id": token.ID}}
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO public.overlay_join_tokens").
		WithArgs(token.ID, token.TokenHash, token.UserID, token.NetworkID, "", "", token.RemainingUses, token.ExpiresAt).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectQuery("INSERT INTO public.audit_logs").
		WithArgs(sqlmock.AnyArg(), audit.Action, audit.ActorUUID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	if err := st.CreateOverlayJoinToken(context.Background(), token, audit); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayJoinMigrationLocksOneTimeHashOnlyContract(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "sql", "migrations", "2026082802_overlay_join_enrollment.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{
		"token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32)",
		"remaining_uses IN (0, 1)",
		"overlay_enrollment_sessions_join_device_uk",
		"overlay_enrollment_sessions_device_fk",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("migration missing %q", required)
		}
	}
	for _, forbidden := range []string{"raw_token", "token_secret", "enrollment_secret"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("migration persists forbidden field %q", forbidden)
		}
	}
}

func TestPostgresExchangeLocksLastUseAndAtomicallyRegisters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	joinHash := sha256.Sum256([]byte("xjt_secret"))
	sessionHash := sha256.Sum256([]byte("xenr_secret"))
	userID := "11111111-1111-1111-1111-111111111111"
	exchange := &OverlayJoinExchange{
		JoinTokenHash: joinHash[:],
		Enrollment:    OverlayEnrollmentSession{ID: "enr-1", TokenHash: sessionHash[:], CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)},
		Device:        OverlayDevice{ID: "dev-1", Name: "dev-1", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		AddressPrefix: "172.29.10", AddressStartHost: 100, AddressEndHost: 254,
	}
	audit := &AuditLog{Action: AuditActionOverlayJoinExchange, Details: map[string]any{}}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM public.overlay_join_tokens WHERE token_hash = $1 FOR UPDATE")).
		WithArgs(exchange.JoinTokenHash).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_uuid", "network_id", "device_id", "platform", "remaining_uses", "expires_at", "revoked_at", "created_at", "last_exchanged_at"}).
			AddRow("join-1", userID, "net", nil, nil, 1, now.Add(time.Hour), nil, now.Add(-time.Minute), nil))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM public.overlay_enrollment_sessions").
		WithArgs("join-1", exchange.Device.ID).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("net").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT network_id, platform, wireguard_public_key, wireguard_address").
		WithArgs(userID, exchange.Device.ID).
		WillReturnRows(sqlmock.NewRows([]string{"network_id", "platform", "wireguard_public_key", "wireguard_address"}))
	mock.ExpectQuery("SELECT wireguard_address FROM public.overlay_devices").WithArgs("net").
		WillReturnRows(sqlmock.NewRows([]string{"wireguard_address"}))
	mock.ExpectQuery("INSERT INTO public.overlay_devices").
		WithArgs(exchange.Device.ID, userID, "net", exchange.Device.Name, exchange.Device.Platform, exchange.Device.Hostname,
			exchange.Device.WireGuardPublicKey, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).AddRow(now, now))
	mock.ExpectExec("UPDATE public.overlay_join_tokens").WithArgs("join-1", now).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO public.overlay_enrollment_sessions").
		WithArgs(exchange.Enrollment.ID, exchange.Enrollment.TokenHash, "join-1", userID, "net", exchange.Device.ID,
			exchange.Device.Platform, exchange.Device.WireGuardPublicKey, exchange.Enrollment.ExpiresAt, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO public.audit_logs").
		WithArgs(sqlmock.AnyArg(), audit.Action, nil, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(now))
	mock.ExpectCommit()
	if err := st.ExchangeOverlayJoinToken(context.Background(), exchange, audit); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
