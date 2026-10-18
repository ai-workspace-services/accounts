package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Run the identical reservation/association contract against memory and a real
// PostgreSQL instance. The database must be a fresh, disposable local database.
func TestFinanceRefundOperationsMemory(t *testing.T) {
	financeRefundOperationsContract(t, NewMemoryStore(), nil)
}

func TestFinanceRefundOperationsPostgres(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_FINANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("set ACCOUNTS_FINANCE_TEST_DSN to a fresh local p0f_refund_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || !strings.HasPrefix(cfg.Database, "p0f_refund_test") {
		t.Fatal("only disposable local p0f_refund_test is accepted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("requires fresh empty database: tables=%d err=%v", tables, err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE public.users (uuid uuid PRIMARY KEY); CREATE TABLE public.subscriptions (uuid uuid PRIMARY KEY, user_uuid uuid NOT NULL REFERENCES public.users(uuid));`)
	if err != nil {
		t.Fatal(err)
	}
	// Actual checked-in migrations retain constraints and append-only triggers.
	for _, migration := range []string{"2026092801_local_finance_ledger.up.sql", "2026101301_referral_program.up.sql"} {
		body, err := os.ReadFile("../../sql/migrations/" + migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	financeRefundOperationsContract(t, &postgresStore{db: db}, db)
}

func financeRefundOperationsContract(t *testing.T, st Store, db *sql.DB) {
	ctx := context.Background()
	fixture := func(t *testing.T) *FinancePayment {
		t.Helper()
		accountID, subID := uuid.NewString(), uuid.NewString()
		if db != nil {
			if _, err := db.ExecContext(ctx, `INSERT INTO public.users(uuid) VALUES ($1)`, accountID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO public.subscriptions(uuid,user_uuid) VALUES ($1,$2)`, subID, accountID); err != nil {
				t.Fatal(err)
			}
		} else {
			user := &User{Name: "refund-" + uuid.NewString(), Email: uuid.NewString() + "@example.invalid"}
			if err := st.CreateUser(ctx, user); err != nil {
				t.Fatal(err)
			}
			sub := &Subscription{UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_" + uuid.NewString()}
			if err := st.UpsertSubscription(ctx, sub); err != nil {
				t.Fatal(err)
			}
			accountID, subID = user.ID, sub.ID
		}
		invoice := &FinanceInvoice{IdempotencyKey: uuid.NewString(), AccountUUID: accountID, SubscriptionUUID: subID, Provider: "stripe", ProviderInvoiceID: "in_" + uuid.NewString(), AmountMinor: 1000, Currency: "USD"}
		if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
			t.Fatal(err)
		}
		payment := &FinancePayment{IdempotencyKey: uuid.NewString(), InvoiceID: invoice.ID, AccountUUID: accountID, Provider: "stripe", ProviderPaymentID: "pi_" + uuid.NewString(), AmountMinor: 1000, Currency: "USD"}
		if _, err := st.RecordFinancePayment(ctx, payment); err != nil {
			t.Fatal(err)
		}
		return payment
	}
	operation := func(payment *FinancePayment, amount int64) *FinanceOperation {
		return &FinanceOperation{IdempotencyKey: "subscription-refund:" + uuid.NewString(), OperationType: "refund", TargetType: "payment", TargetID: payment.ID, Provider: "stripe", Request: json.RawMessage(fmt.Sprintf(`{"amount_minor":%d,"currency":"USD"}`, amount))}
	}
	markStale := func(t *testing.T, op *FinanceOperation) {
		t.Helper()
		if db != nil {
			if _, err := db.ExecContext(ctx, `UPDATE public.finance_operations SET updated_at = now() - interval '3 minutes' WHERE id = $1`, op.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			ms := st.(*memoryStore)
			ms.mu.Lock()
			ms.financeOperations[op.ID].UpdatedAt = time.Now().Add(-3 * time.Minute)
			ms.mu.Unlock()
		}
	}

	t.Run("webhook_request_race_one_fact_no_double_reservation", func(t *testing.T) {
		payment := fixture(t)
		op := operation(payment, 1000)
		if claimed, err := st.BeginFinanceOperation(ctx, op); err != nil || !claimed {
			t.Fatal(err)
		}
		var inserted atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fact := &FinanceRefund{IdempotencyKey: op.IdempotencyKey, PaymentID: payment.ID, Provider: "stripe", ProviderRefundID: "re_" + op.ID, AmountMinor: 1000, Currency: "USD", RefundedAt: time.Unix(1770000000, 0)}
				created, err := st.RecordFinanceRefund(ctx, fact)
				if err != nil {
					t.Errorf("concurrent fact: %v", err)
				}
				if created {
					inserted.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if inserted.Load() != 1 {
			t.Fatalf("inserted=%d", inserted.Load())
		}
		stored, err := st.GetFinanceOperation(ctx, op.IdempotencyKey)
		if err != nil || stored.ProviderOperationID != "re_"+op.ID || stored.Status != FinanceOperationInProgress {
			t.Fatalf("provider association not atomic: %+v %v", stored, err)
		}
		if err := st.FinishFinanceOperation(ctx, op.ID, FinanceOperationReconciliationRequired, "", nil, errors.New("cancel failed"), nil, op.AttemptCount); err != nil {
			t.Fatal(err)
		}
		stored, _ = st.GetFinanceOperation(ctx, op.IdempotencyKey)
		if stored.Status != FinanceOperationReconciliationRequired || stored.LastError != "cancel failed" || stored.ProviderOperationID != "re_"+op.ID {
			t.Fatalf("lost reconcile reservation/reference: %+v", stored)
		}
		if claimed, err := st.BeginFinanceOperation(ctx, op); err != nil || !claimed {
			t.Fatalf("own materialized fact blocked retry: %v", err)
		}
		if err := st.FinishFinanceOperation(ctx, op.ID, FinanceOperationSucceeded, "re_"+op.ID, nil, nil, nil, op.AttemptCount); err != nil {
			t.Fatal(err)
		}
		if _, err := st.BeginFinanceOperation(ctx, operation(payment, 1)); !errors.Is(err, ErrFinanceRefundExceedsPayment) {
			t.Fatalf("over-refund allowed: %v", err)
		}
		events, err := st.ListFinanceOperationEvents(ctx, op.ID)
		if err != nil || len(events) != 5 || events[1].EventType != "refund_recorded" {
			t.Fatalf("events=%+v err=%v", events, err)
		}
	})

	t.Run("concurrent_pending_and_partial_operations_reserve_capacity", func(t *testing.T) {
		payment := fixture(t)
		var claims atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		var opsMu sync.Mutex
		var ops []*FinanceOperation
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				op := operation(payment, 200)
				claimed, err := st.BeginFinanceOperation(ctx, op)
				if claimed && err == nil {
					claims.Add(1)
					opsMu.Lock()
					ops = append(ops, op)
					opsMu.Unlock()
				} else if !errors.Is(err, ErrFinanceRefundExceedsPayment) {
					t.Errorf("claim: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if claims.Load() != 5 {
			t.Fatalf("claims=%d", claims.Load())
		}
		fact := &FinanceRefund{IdempotencyKey: ops[0].IdempotencyKey, PaymentID: payment.ID, Provider: "stripe", ProviderRefundID: "re_" + ops[0].ID, AmountMinor: 200, Currency: "USD"}
		if _, err := st.RecordFinanceRefund(ctx, fact); err != nil {
			t.Fatalf("own reservation counted twice: %v", err)
		}
		if _, err := st.RecordFinanceRefund(ctx, &FinanceRefund{IdempotencyKey: uuid.NewString(), PaymentID: payment.ID, Provider: "stripe", ProviderRefundID: "re_unrelated", AmountMinor: 1, Currency: "USD"}); !errors.Is(err, ErrFinanceRefundExceedsPayment) {
			t.Fatalf("other pending reservations skipped: %v", err)
		}
		wrong := *fact
		wrong.IdempotencyKey = ops[1].IdempotencyKey
		wrong.ProviderRefundID = "re_wrong"
		wrong.AmountMinor = 201
		if _, err := st.RecordFinanceRefund(ctx, &wrong); !errors.Is(err, ErrFinanceIdempotencyConflict) {
			t.Fatalf("mismatching fact accepted: %v", err)
		}
	})

	t.Run("stale_claim_recovery_fences_old_result_and_keeps_unknown_reserved", func(t *testing.T) {
		payment := fixture(t)
		op := operation(payment, 1000)
		if _, err := st.BeginFinanceOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err := st.CheckpointFinanceOperation(ctx, op.ID, 1, json.RawMessage(`{"provider_request_started":true}`)); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := st.GetFinanceOperation(ctx, op.IdempotencyKey)
		if err != nil || !strings.Contains(string(checkpoint.Response), "true") {
			t.Fatalf("checkpoint not durable: %+v %v", checkpoint, err)
		}
		if _, err := st.BeginFinanceOperation(ctx, op); !errors.Is(err, ErrFinanceOperationInProgress) {
			t.Fatal(err)
		}
		markStale(t, op)
		if claimed, err := st.BeginFinanceOperation(ctx, op); err != nil || !claimed || op.AttemptCount != 2 {
			t.Fatalf("crash recovery claim=%v attempt=%d err=%v", claimed, op.AttemptCount, err)
		}
		if err := st.FinishFinanceOperation(ctx, op.ID, FinanceOperationSucceeded, "re_old_worker", nil, nil, nil, 1); !errors.Is(err, ErrFinanceOperationNotRetryable) {
			t.Fatalf("stale result was not fenced: %v", err)
		}
		if err := st.CheckpointFinanceOperation(ctx, op.ID, 1, json.RawMessage(`{"provider_request_started":false}`)); !errors.Is(err, ErrFinanceOperationNotRetryable) {
			t.Fatalf("stale checkpoint not fenced: %v", err)
		}
		if err := st.FinishFinanceOperation(ctx, op.ID, FinanceOperationReconciliationRequired, "", nil, errors.New("unknown result"), nil, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := st.BeginFinanceOperation(ctx, operation(payment, 1)); !errors.Is(err, ErrFinanceRefundExceedsPayment) {
			t.Fatalf("unknown outcome lost reservation: %v", err)
		}
		operations, err := st.ListFinanceOperationsForReconciliation(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range operations {
			if item.ID == op.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("missing recovery projection")
		}
	})

	if db != nil {
		t.Run("fact_reference_and_event_roll_back_together", func(t *testing.T) {
			payment := fixture(t)
			op := operation(payment, 1000)
			if _, err := st.BeginFinanceOperation(ctx, op); err != nil {
				t.Fatal(err)
			}
			_, err := db.ExecContext(ctx, `CREATE FUNCTION public.refund_event_failure_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type IN ('refund_recorded', 'attempt_checkpoint') THEN RAISE EXCEPTION 'injected event storage failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER refund_event_failure_fixture BEFORE INSERT ON public.finance_operation_events FOR EACH ROW EXECUTE FUNCTION public.refund_event_failure_fixture();`)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CheckpointFinanceOperation(ctx, op.ID, op.AttemptCount, json.RawMessage(`{"provider_request_started":true}`)); err == nil {
				t.Fatal("expected checkpoint event storage failure")
			}
			checkpoint, err := st.GetFinanceOperation(ctx, op.IdempotencyKey)
			if err != nil || !jsonEqual(checkpoint.Response, json.RawMessage(`{}`)) {
				t.Fatalf("checkpoint survived rollback: %+v %v", checkpoint, err)
			}
			fact := &FinanceRefund{IdempotencyKey: op.IdempotencyKey, PaymentID: payment.ID, Provider: "stripe", ProviderRefundID: "re_" + op.ID, AmountMinor: 1000, Currency: "USD"}
			if _, err := st.RecordFinanceRefund(ctx, fact); err == nil {
				t.Fatal("expected event persistence failure")
			}
			stored, err := st.GetFinanceOperation(ctx, op.IdempotencyKey)
			if err != nil || stored.ProviderOperationID != "" {
				t.Fatalf("binding survived rollback: %+v %v", stored, err)
			}
			facts, err := st.ListFinanceRefunds(ctx, payment.AccountUUID, 0)
			if err != nil || len(facts) != 0 {
				t.Fatalf("fact survived rollback: %+v %v", facts, err)
			}
			if _, err := db.ExecContext(ctx, `DROP TRIGGER refund_event_failure_fixture ON public.finance_operation_events; DROP FUNCTION public.refund_event_failure_fixture();`); err != nil {
				t.Fatal(err)
			}
			if inserted, err := st.RecordFinanceRefund(ctx, fact); err != nil || !inserted {
				t.Fatalf("retry after rollback: inserted=%v err=%v", inserted, err)
			}
		})
		t.Run("refund_and_events_remain_append_only", func(t *testing.T) {
			for _, table := range []string{"finance_refunds", "finance_operation_events"} {
				// The existing 2026101301 polymorphic trigger can report a missing
				// service_period_start field instead of its intended append-only
				// error on these tables. Either error must leave all rows intact.
				var before, after int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public."+table).Scan(&before); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, "DELETE FROM public."+table); err == nil {
					t.Fatalf("%s mutation guard: %v", table, err)
				}
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public."+table).Scan(&after); err != nil || after != before {
					t.Fatalf("%s rows changed: before=%d after=%d err=%v", table, before, after, err)
				}
			}
		})
	}
}
