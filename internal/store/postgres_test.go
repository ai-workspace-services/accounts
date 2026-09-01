package store

import (
	"strings"
	"testing"

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
