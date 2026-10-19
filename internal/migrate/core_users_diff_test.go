package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCoreUserDiffBoundaries(t *testing.T) {
	source := map[string]businessUser{"user@example.invalid": {ID: "source-id", Email: "user@example.invalid", Proxy: "proxy", PasswordHash: "password"}}
	for _, tc := range []struct {
		name   string
		target map[string]businessUser
		want   CoreUsersDiff
	}{
		{"target identity retained", map[string]businessUser{"user@example.invalid": {ID: "different-target-id", Email: "user@example.invalid", Proxy: "proxy", PasswordHash: "password"}}, CoreUsersDiff{Equal: true}},
		{"missing", map[string]businessUser{}, CoreUsersDiff{SourceOnlyUsers: 1}},
		{"both unique", map[string]businessUser{"other@example.invalid": {Email: "other@example.invalid"}}, CoreUsersDiff{SourceOnlyUsers: 1, TargetOnlyUsers: 1}},
		{"credential differences", map[string]businessUser{"user@example.invalid": {Email: " User@example.invalid ", Proxy: "other", PasswordHash: "changed"}}, CoreUsersDiff{EmailMismatches: 1, ProxyUUIDMismatches: 1, PasswordHashMismatches: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffCoreUsers(source, tc.target); got != tc.want {
				t.Fatalf("diff = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCoreUserMatchingRejectsAmbiguityAndRedactsErrors(t *testing.T) {
	const id1 = "00000000-0000-0000-0000-000000000001"
	const id2 = "00000000-0000-0000-0000-000000000002"
	const proxy1 = "00000000-0000-0000-0000-000000000011"
	const proxy2 = "00000000-0000-0000-0000-000000000012"
	for _, tc := range []struct {
		name, email, secondEmail, secondProxy string
		ok                                    bool
	}{
		{"normalized", " User@example.invalid ", "other@example.invalid", proxy2, true},
		{"empty email", " ", "other@example.invalid", proxy2, false},
		{"duplicate normalized email", " User@example.invalid ", "user@EXAMPLE.invalid", proxy2, false},
		{"duplicate proxy", "user@example.invalid", "other@example.invalid", proxy1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT uuid::text,email,password,proxy_uuid::text FROM public.users ORDER BY uuid").WillReturnRows(
				sqlmock.NewRows([]string{"uuid", "email", "password", "proxy_uuid"}).
					AddRow(id1, tc.email, "private-password-fixture", proxy1).
					AddRow(id2, tc.secondEmail, "private-password-fixture", tc.secondProxy))
			mock.ExpectRollback()
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			users, err := businessUsers(context.Background(), tx)
			if tc.ok {
				if err != nil || len(users) != 2 || users["user@example.invalid"].ID != id1 {
					t.Fatal("normalized users did not match")
				}
			} else if err == nil {
				t.Fatal("ambiguous identity was accepted")
			} else if strings.Contains(err.Error(), "example.invalid") || strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), id1) {
				t.Fatal("identity validation error contains private values")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
