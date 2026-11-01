package store

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestActivationOnlyChangesAccountAccessAndAuditsOnce(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	user := &User{Name: "Sample", Email: "sample@example.com", EmailVerified: true, MFAEnabled: true, Groups: []string{"User", MonthlyFreeQuotaLimitGroup}, PasswordHash: "fixture", Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetUserByID(ctx, user.ID)
	for i := 0; i < 2; i++ {
		if err := st.SetUserActiveWithAudit(ctx, user.ID, false, "admin-id"); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := st.GetUserByID(ctx, user.ID)
	after.Active = before.Active
	after.UpdatedAt = before.UpdatedAt
	if !reflect.DeepEqual(before, after) {
		t.Fatal("deactivation changed unrelated account fields")
	}
	entries, err := st.ListAuditLogs(ctx, AuditLogFilter{})
	if err != nil || len(entries) != 1 || entries[0].Action != "account.user.deactivated" || entries[0].ActorUUID != "admin-id" {
		t.Fatalf("expected one operator audit: %v %v", entries, err)
	}
}
func TestPostgresActivationRollsBackIfAuditFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT active, archived_at FROM users WHERE uuid = $1 FOR UPDATE`)).WithArgs("user-id").WillReturnRows(sqlmock.NewRows([]string{"active", "archived_at"}).AddRow(true, nil))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET active = $1, updated_at = now() WHERE uuid = $2`)).WithArgs(false, "user-id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO audit_logs").WillReturnError(errors.New("audit unavailable"))
	mock.ExpectRollback()
	if err := st.SetUserActiveWithAudit(context.Background(), "user-id", false, "actor-id"); err == nil {
		t.Fatal("activation succeeded without audit")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestGroupEditorCannotGrantVerificationBypass(t *testing.T) {
	if got := PreserveRegistrationGroups([]string{"User"}, []string{"User", EmailOptionalRegistrationGroup}); reflect.DeepEqual(got, []string{"User", EmailOptionalRegistrationGroup}) {
		t.Fatal("group editor granted verification bypass")
	}
	if got := PreserveRegistrationGroups([]string{"User", EmailOptionalRegistrationGroup}, []string{"User"}); !reflect.DeepEqual(got, []string{"User", EmailOptionalRegistrationGroup}) {
		t.Fatal("group editor removed registration marker")
	}
}

// A concurrent insert can win after both prechecks passed. Map PostgreSQL's
// unique violation to the same domain error as a sequential duplicate.
func TestPostgresRegistrationUniqueViolationAfterPrecheck(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db, capsLoaded: true}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM users WHERE lower(email) = $1)")).WithArgs("same@example.com").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM users WHERE lower(username) = lower($1))")).WithArgs("UniqueName").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("INSERT INTO users").WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "users_email_lower_uk"})
	err = st.CreateUser(context.Background(), &User{Name: "UniqueName", Email: " SAME@example.com ", Active: true})
	if !errors.Is(err, ErrEmailExists) {
		t.Fatalf("concurrent duplicate must map to ErrEmailExists: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
