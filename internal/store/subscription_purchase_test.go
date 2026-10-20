package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubscriptionPurchaseMemory(t *testing.T) {
	subscriptionPurchaseContract(t, NewMemoryStore(), nil)
}

func TestSubscriptionPurchasePostgres(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_SUBSCRIPTION_TEST_DSN")
	if dsn == "" {
		t.Skip("set ACCOUNTS_SUBSCRIPTION_TEST_DSN to a disposable local subscription_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || !strings.HasPrefix(cfg.Database, "subscription_test") {
		t.Fatal("requires disposable local subscription_test database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables int
	if err = db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal("requires empty database", tables, err)
	}
	_, err = db.Exec(`CREATE TABLE public.users(uuid uuid PRIMARY KEY); CREATE TABLE public.subscriptions(uuid uuid PRIMARY KEY,user_uuid uuid NOT NULL REFERENCES public.users(uuid));
 CREATE TABLE public.account_billing_profiles(account_uuid uuid PRIMARY KEY,included_quota_bytes bigint NOT NULL,pricing_rule_version text NOT NULL DEFAULT '',updated_at timestamptz DEFAULT now());
 CREATE TABLE public.account_quota_states(account_uuid uuid PRIMARY KEY,remaining_included_quota bigint NOT NULL,period_start timestamptz,period_end timestamptz,effective_at timestamptz DEFAULT now(),updated_at timestamptz DEFAULT now());`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../sql/migrations/2026092801_local_finance_ledger.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	subscriptionPurchaseContract(t, &postgresStore{db: db}, db)
}

func subscriptionPurchaseContract(t *testing.T, st Store, db *sql.DB) {
	ctx := context.Background()
	id := uuid.NewString()
	if db != nil {
		if _, err := db.Exec(`INSERT INTO users(uuid) VALUES($1)`, id); err != nil {
			t.Fatal(err)
		}
	} else {
		u := &User{Name: "Subscription Test", Email: "subscription-test@example.invalid"}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		id = u.ID
	}
	var wg sync.WaitGroup
	var winners atomic.Int32
	var operation *FinanceOperation
	var mu sync.Mutex
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := st.ReserveSubscriptionPurchase(ctx, id, json.RawMessage(`{"planId":"example"}`))
			if err == nil {
				winners.Add(1)
				mu.Lock()
				operation = op
				mu.Unlock()
			} else if !errors.Is(err, ErrFinanceOperationInProgress) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("reservation winners", winners.Load())
	}
	if err := st.UpdateSubscriptionPurchase(ctx, operation.ID, "cs_fixture", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveSubscriptionPurchase(ctx, id, nil); !errors.Is(err, ErrFinanceOperationInProgress) {
		t.Fatal("lost durable pending gate", err)
	}
	if err := st.UpdateSubscriptionPurchase(ctx, operation.ID, "cs_other", nil, true); !errors.Is(err, ErrFinanceIdempotencyConflict) {
		t.Fatal("accepted provider collision", err)
	}
	if err := st.UpdateSubscriptionPurchase(ctx, operation.ID, "cs_fixture", nil, true); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSubscriptionPurchase(ctx, operation.ID, "cs_fixture", nil, false); err != nil {
		t.Fatal(err)
	}
	if pending, err := st.PendingSubscriptionPurchase(ctx, id); err != nil || pending != nil {
		t.Fatal("terminal state regressed", err)
	}

	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	end := start.AddDate(0, 1, 0)
	if db != nil {
		if _, err := db.Exec(`INSERT INTO account_billing_profiles(account_uuid,included_quota_bytes) VALUES($1,20)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO account_quota_states(account_uuid,remaining_included_quota,period_start,period_end) VALUES($1,14,$2,$3)`, id, start, end); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: id, IncludedQuotaBytes: 20}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: id, RemainingIncludedQuota: 14, PeriodStart: &start, PeriodEnd: &end}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpgradeSubscriptionQuota(ctx, id, 20, 50, start.Add(-time.Hour), end); !errors.Is(err, ErrSubscriptionQuotaConflict) {
		t.Fatal("accepted period conflict", err)
	}
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.UpgradeSubscriptionQuota(ctx, id, 20, 50, start, end); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var remaining, included int64
	if db != nil {
		if err := db.QueryRow(`SELECT remaining_included_quota,included_quota_bytes FROM account_quota_states JOIN account_billing_profiles USING(account_uuid) WHERE account_uuid=$1`, id).Scan(&remaining, &included); err != nil {
			t.Fatal(err)
		}
	} else {
		q, _ := st.GetAccountQuotaState(ctx, id)
		p, _ := st.GetAccountBillingProfile(ctx, id)
		remaining, included = q.RemainingIncludedQuota, p.IncludedQuotaBytes
	}
	if remaining != 44 || included != 50 {
		t.Fatalf("allowances stacked or usage reset: remaining=%d included=%d", remaining, included)
	}
}
