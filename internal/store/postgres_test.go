package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFormatIdentifier(t *testing.T) {
	id := uuid.New()
	arr := [16]byte(id)
	ptrArr := new([16]byte)
	*ptrArr = arr
	pgUUID := pgtype.UUID{Bytes: arr, Valid: true}

	cases := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{name: "string", value: id.String(), want: id.String()},
		{name: "byte array", value: arr, want: id.String()},
		{name: "byte array pointer", value: ptrArr, want: id.String()},
		{name: "pgtype uuid", value: pgUUID, want: id.String()},
		{name: "pgtype uuid pointer", value: &pgUUID, want: id.String()},
		{name: "nil pointer", value: (*pgtype.UUID)(nil), wantErr: true},
		{name: "invalid pgtype", value: pgtype.UUID{}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := formatIdentifier(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil value %q", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestSelectUserQuery(t *testing.T) {
	store := &postgresStore{}
	tests := []struct {
		name string
		caps schemaCapabilities
		want string
	}{
		{
			name: "no mfa columns",
			caps: schemaCapabilities{},
			want: "NULL::text",
		},
		{
			name: "with mfa columns",
			caps: schemaCapabilities{
				hasMFATOTPSecret:     true,
				hasMFAEnabled:        true,
				hasMFASecretIssuedAt: true,
				hasMFAConfirmedAt:    true,
				hasCreatedAt:         true,
				hasUpdatedAt:         true,
			},
			want: "mfa_totp_secret",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query := store.selectUserQuery(tc.caps, "WHERE uuid = $1")
			if !strings.Contains(query, tc.want) {
				t.Fatalf("expected query to contain %q, got %q", tc.want, query)
			}
		})
	}
}

func TestSelectUserQueryUsesVerificationTimestamp(t *testing.T) {
	query := (&postgresStore{}).selectUserQuery(schemaCapabilities{}, "WHERE uuid = $1")
	if !strings.Contains(query, "(email_verified_at IS NOT NULL) AS email_verified") {
		t.Fatalf("user reads must derive email verification from email_verified_at, got %q", query)
	}
}

func TestPostgresDeleteUserFailsClosedWithoutLifecycleSchema(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()
	st := &postgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT\s+EXISTS.*account_lifecycle_events`).
		WillReturnRows(sqlmock.NewRows([]string{"schema_ready"}).AddRow(false))
	mock.ExpectRollback()

	err = st.DeleteUser(context.Background(), "user-1", userArchiveTestAudit("user-1"), "request-1")
	if !errors.Is(err, ErrUserArchiveUnsupported) {
		t.Fatalf("expected lifecycle schema dependency error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestPostgresDeleteUserRollsBackWhenAuditInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	const userID = "11111111-1111-4111-8111-111111111111"
	const actorID = "22222222-2222-4222-8222-222222222222"
	audit := &AuditLog{
		Action: AuditActionUserArchive, ActorUUID: actorID,
		Details: map[string]any{"target_uuid": userID, "reason": "retention request"},
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT\s+EXISTS.*account_lifecycle_events`).
		WillReturnRows(sqlmock.NewRows([]string{"schema_ready"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT account_lifecycle_state, role, level, groups, archived_at\n\t\tFROM public.users WHERE uuid = $1 FOR UPDATE")).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"account_lifecycle_state", "role", "level", "groups", "archived_at"}).
			AddRow("active", RoleUser, LevelUser, []byte("[]"), nil))
	mock.ExpectQuery(`(?s)SELECT e\.transition_id::text.*e\.request_id = \$2`).
		WithArgs(userID, "request-1", AuditActionUserArchive).
		WillReturnRows(sqlmock.NewRows([]string{"transition_id", "actor_ref", "reason", "metadata", "audit_uuid", "audit_actor", "audit_details", "created_at"}))
	mock.ExpectQuery(`(?s)SELECT EXISTS\s+\(\s+SELECT 1 FROM public\.subscriptions`).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`(?s)UPDATE public\.users\s+SET active = FALSE`).
		WithArgs(sqlmock.AnyArg(), actorID, "retention request", sqlmock.AnyArg(), userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO public\.account_lifecycle_events`).
		WithArgs(sqlmock.AnyArg(), userID, "active", actorID, "retention request", "request-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO public\.audit_logs`).
		WillReturnError(errors.New("audit store unavailable"))
	mock.ExpectRollback()

	err = st.DeleteUser(context.Background(), userID, audit, "request-1")
	if err == nil || !strings.Contains(err.Error(), "audit store unavailable") {
		t.Fatalf("expected audit insert error, got %v", err)
	}
	if !audit.CreatedAt.IsZero() {
		t.Fatal("failed transaction must not report a committed audit timestamp")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestPostgresDeleteUserReplaysExistingRequest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	const userID = "11111111-1111-4111-8111-111111111111"
	const actorID = "22222222-2222-4222-8222-222222222222"
	const transitionID = "33333333-3333-4333-8333-333333333333"
	const auditID = "44444444-4444-4444-8444-444444444444"
	const requestID = "request-1"
	at := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	details := map[string]any{
		"target_uuid": userID, "reason": "retention request", "request_id": requestID,
		"transition_id": transitionID,
	}
	metadata, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("marshal replay details: %v", err)
	}
	audit := &AuditLog{
		Action: AuditActionUserArchive, ActorUUID: actorID,
		Details: map[string]any{"target_uuid": userID, "reason": "retention request"},
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT\s+EXISTS.*account_lifecycle_events`).
		WillReturnRows(sqlmock.NewRows([]string{"schema_ready"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT account_lifecycle_state, role, level, groups, archived_at\n\t\tFROM public.users WHERE uuid = $1 FOR UPDATE")).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"account_lifecycle_state", "role", "level", "groups", "archived_at"}).
			AddRow("archived", RoleUser, LevelUser, []byte("[]"), at))
	mock.ExpectQuery(`(?s)SELECT e\.transition_id::text.*e\.request_id = \$2`).
		WithArgs(userID, requestID, AuditActionUserArchive).
		WillReturnRows(sqlmock.NewRows([]string{"transition_id", "actor_ref", "reason", "metadata", "audit_uuid", "audit_actor", "audit_details", "created_at"}).
			AddRow(transitionID, actorID, "retention request", metadata, auditID, actorID, metadata, at))
	mock.ExpectCommit()

	if err := st.DeleteUser(context.Background(), userID, audit, requestID); err != nil {
		t.Fatalf("replay completed archive: %v", err)
	}
	if audit.UUID != auditID || audit.CreatedAt != at || audit.Details["transition_id"] != transitionID {
		t.Fatalf("expected original archive result, got %+v", audit)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
