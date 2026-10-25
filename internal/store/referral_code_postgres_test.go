package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestReferralCodePostgresCollision(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_REFERRAL_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNTS_REFERRAL_TEST_DSN_REQUIRED") == "1" {
			t.Fatal("isolated PostgreSQL DSN required")
		}
		t.Skip("set ACCOUNTS_REFERRAL_TEST_DSN for isolated PostgreSQL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") || cfg.Database != "referral_store_test" {
		t.Fatal("only local referral_store_test allowed")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := uuid.NewString(), uuid.NewString()
	for _, id := range []string{a, b} {
		if _, err := db.ExecContext(ctx, `INSERT INTO public.users(uuid,email) VALUES($1,$2)`, id, id+"@example.invalid"); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM public.referral_codes WHERE user_uuid IN ($1,$2)`, a, b)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM public.users WHERE uuid IN ($1,$2)`, a, b)
	}()
	st := &postgresStore{db: db}
	code := "C" + uuid.NewString()[:8]
	if _, err := st.EnsureReferralCode(ctx, a, code); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureReferralCode(ctx, b, code); !errors.Is(err, ErrReferralAlreadyAttributed) {
		t.Fatalf("collision=%v", err)
	}
	replacement := "R" + uuid.NewString()[:8]
	got, err := st.EnsureReferralCode(ctx, b, replacement)
	if err != nil || got.Code != strings.ToUpper(replacement) {
		t.Fatalf("retry=%+v err=%v", got, err)
	}
	got, err = st.EnsureReferralCode(ctx, b, code)
	if err != nil || got.Code != strings.ToUpper(replacement) {
		t.Fatalf("existing owner changed=%+v err=%v", got, err)
	}
	if _, err := st.EnsureReferralCode(ctx, uuid.NewString(), "F"+uuid.NewString()[:8]); err == nil || errors.Is(err, ErrReferralAlreadyAttributed) {
		t.Fatalf("foreign-key error misclassified: %v", err)
	}
}
