package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func financeTestAccount(t *testing.T, st Store) (string, string) {
	t.Helper()
	user := &User{Name: "finance-test", Email: "finance-test@example.invalid"}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create finance account: %v", err)
	}
	sub := &Subscription{UserID: user.ID, ExternalID: "subscription-test", Provider: "local", Status: "active"}
	if err := st.UpsertSubscription(context.Background(), sub); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return user.ID, sub.ID
}

func TestFinanceLedgerIdempotencyRefundsAndAccountQueries(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	accountID, subscriptionID := financeTestAccount(t, st)
	invoice := &FinanceInvoice{
		IdempotencyKey: "invoice:local-1", AccountUUID: accountID, SubscriptionUUID: subscriptionID,
		AmountMinor: 1250, Currency: "usd", Description: "monthly", Provider: "stripe", ProviderInvoiceID: "inv_123",
	}
	inserted, err := st.CreateFinanceInvoice(ctx, invoice)
	if err != nil || !inserted {
		t.Fatalf("create invoice: inserted=%v err=%v", inserted, err)
	}
	replay := *invoice
	replay.ID = ""
	inserted, err = st.CreateFinanceInvoice(ctx, &replay)
	if err != nil || inserted || replay.ID != invoice.ID {
		t.Fatalf("invoice replay should resolve original: inserted=%v id=%q err=%v", inserted, replay.ID, err)
	}
	conflict := *invoice
	conflict.AmountMinor++
	if _, err := st.CreateFinanceInvoice(ctx, &conflict); !errors.Is(err, ErrFinanceIdempotencyConflict) {
		t.Fatalf("changed invoice payload should conflict, got %v", err)
	}

	payment := &FinancePayment{
		IdempotencyKey: "payment:local-1", InvoiceID: invoice.ID, AccountUUID: accountID,
		AmountMinor: 1250, Currency: "USD", Provider: "stripe", ProviderPaymentID: "pi_123",
	}
	if inserted, err := st.RecordFinancePayment(ctx, payment); err != nil || !inserted {
		t.Fatalf("record payment: inserted=%v err=%v", inserted, err)
	}
	wrongCurrency := *payment
	wrongCurrency.ID, wrongCurrency.IdempotencyKey, wrongCurrency.Currency = "", "payment:wrong-currency", "EUR"
	if _, err := st.RecordFinancePayment(ctx, &wrongCurrency); !errors.Is(err, ErrFinanceIdempotencyConflict) {
		t.Fatalf("payment currency mismatch should fail, got %v", err)
	}

	for i, amount := range []int64{400, 500} {
		refund := &FinanceRefund{
			IdempotencyKey: "refund:" + string(rune('a'+i)), PaymentID: payment.ID,
			AmountMinor: amount, Currency: "USD", Provider: "stripe",
		}
		if inserted, err := st.RecordFinanceRefund(ctx, refund); err != nil || !inserted {
			t.Fatalf("record partial refund %d: inserted=%v err=%v", i, inserted, err)
		}
	}
	if _, err := st.RecordFinanceRefund(ctx, &FinanceRefund{IdempotencyKey: "refund:too-much", PaymentID: payment.ID, AmountMinor: 351, Currency: "USD"}); !errors.Is(err, ErrFinanceRefundExceedsPayment) {
		t.Fatalf("over-refund should fail, got %v", err)
	}
	if inserted, err := st.RecordFinanceRefund(ctx, &FinanceRefund{IdempotencyKey: "refund:final", PaymentID: payment.ID, AmountMinor: 350, Currency: "USD"}); err != nil || !inserted {
		t.Fatalf("full remaining refund should succeed: inserted=%v err=%v", inserted, err)
	}
	refunds, err := st.ListFinanceRefunds(ctx, accountID, 20)
	if err != nil || len(refunds) != 3 {
		t.Fatalf("list account refunds: got %d, err=%v", len(refunds), err)
	}
	invoices, err := st.ListFinanceInvoices(ctx, accountID, subscriptionID, 20)
	if err != nil || len(invoices) != 1 || invoices[0].ID != invoice.ID {
		t.Fatalf("list subscription invoices: got %#v, err=%v", invoices, err)
	}
	payments, err := st.ListFinancePayments(ctx, accountID, 20)
	if err != nil || len(payments) != 1 || payments[0].ID != payment.ID {
		t.Fatalf("list account payments: got %#v, err=%v", payments, err)
	}
}

func TestFinanceRefundConcurrentPartialAmountsNeverExceedPayment(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	accountID, _ := financeTestAccount(t, st)
	invoice := &FinanceInvoice{IdempotencyKey: "invoice:concurrent", AccountUUID: accountID, AmountMinor: 1000, Currency: "USD"}
	if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	payment := &FinancePayment{IdempotencyKey: "payment:concurrent", InvoiceID: invoice.ID, AccountUUID: accountID, AmountMinor: 1000, Currency: "USD"}
	if _, err := st.RecordFinancePayment(ctx, payment); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var successes int
	var successMu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.RecordFinanceRefund(ctx, &FinanceRefund{
				IdempotencyKey: "refund:concurrent:" + string(rune('a'+i)), PaymentID: payment.ID,
				AmountMinor: 200, Currency: "USD",
			})
			if err == nil {
				successMu.Lock()
				successes++
				successMu.Unlock()
				return
			}
			if !errors.Is(err, ErrFinanceRefundExceedsPayment) {
				t.Errorf("unexpected refund result: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes != 5 {
		t.Fatalf("expected five refunds totaling the payment amount, got %d", successes)
	}
}

func TestFinanceRefundProviderOperationsReserveAmountUntilFactIsRecorded(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	accountID, _ := financeTestAccount(t, st)
	invoice := &FinanceInvoice{IdempotencyKey: "invoice:reserved", AccountUUID: accountID, AmountMinor: 1000, Currency: "USD"}
	if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	payment := &FinancePayment{IdempotencyKey: "payment:reserved", InvoiceID: invoice.ID, AccountUUID: accountID, AmountMinor: 1000, Currency: "USD"}
	if _, err := st.RecordFinancePayment(ctx, payment); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		key := "refund-op:" + string(rune('a'+i))
		operation := &FinanceOperation{
			IdempotencyKey: key, OperationType: "refund", TargetType: "payment", TargetID: payment.ID,
			Provider: "stripe", Request: json.RawMessage(`{"amount_minor":200,"currency":"USD"}`),
		}
		if claimed, err := st.BeginFinanceOperation(ctx, operation); err != nil || !claimed {
			t.Fatalf("begin reserved refund %d: claimed=%v err=%v", i, claimed, err)
		}
	}
	if _, err := st.BeginFinanceOperation(ctx, &FinanceOperation{
		IdempotencyKey: "refund-op:overflow", OperationType: "refund", TargetType: "payment", TargetID: payment.ID,
		Provider: "stripe", Request: json.RawMessage(`{"amount_minor":1,"currency":"USD"}`),
	}); !errors.Is(err, ErrFinanceRefundExceedsPayment) {
		t.Fatalf("pending provider operations must reserve refund value: %v", err)
	}
	if err := st.FinishFinanceOperation(ctx, "", FinanceOperationFailed, "", nil, errors.New("irrelevant"), nil); !errors.Is(err, ErrFinanceRecordNotFound) {
		t.Fatalf("unknown operation should not change reservation state: %v", err)
	}
	operations, err := st.ListFinanceOperationsForReconciliation(ctx, 10)
	if err != nil || len(operations) != 5 {
		t.Fatalf("list refund operations for recovery: count=%d err=%v", len(operations), err)
	}
	if err := st.FinishFinanceOperation(ctx, operations[0].ID, FinanceOperationSucceeded, "re_001", json.RawMessage(`{"ok":true}`), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordFinanceRefund(ctx, &FinanceRefund{IdempotencyKey: operations[0].IdempotencyKey, PaymentID: payment.ID, AmountMinor: 201, Currency: "USD", Provider: "stripe", ProviderRefundID: "re_001"}); !errors.Is(err, ErrFinanceIdempotencyConflict) {
		t.Fatalf("refund fact must match its successful provider operation: %v", err)
	}
	refund := &FinanceRefund{IdempotencyKey: operations[0].IdempotencyKey, PaymentID: payment.ID, AmountMinor: 200, Currency: "USD", Provider: "stripe", ProviderRefundID: "re_001"}
	if inserted, err := st.RecordFinanceRefund(ctx, refund); err != nil || !inserted {
		t.Fatalf("persist completed provider refund: inserted=%v err=%v", inserted, err)
	}
	for _, pending := range operations[1:] {
		if err := st.FinishFinanceOperation(ctx, pending.ID, FinanceOperationFailed, "", nil, errors.New("provider declined"), nil); err != nil {
			t.Fatal(err)
		}
	}
	remaining := &FinanceOperation{
		IdempotencyKey: "refund-op:remaining", OperationType: "refund", TargetType: "payment", TargetID: payment.ID,
		Provider: "stripe", Request: json.RawMessage(`{"amount_minor":800,"currency":"USD"}`),
	}
	if claimed, err := st.BeginFinanceOperation(ctx, remaining); err != nil || !claimed {
		t.Fatalf("materialized refund should no longer be double-counted in reservations: claimed=%v err=%v", claimed, err)
	}
}

func TestFinanceOperationFailureRetryAndAppendOnlyEvents(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	operation := &FinanceOperation{
		IdempotencyKey: "payment-operation:1", OperationType: "capture", TargetType: "invoice", TargetID: "invoice-id",
		Provider: "stripe", Request: json.RawMessage(`{"amount_minor":250,"currency":"USD"}`),
	}
	claimed, err := st.BeginFinanceOperation(ctx, operation)
	if err != nil || !claimed || operation.AttemptCount != 1 || operation.Status != FinanceOperationInProgress {
		t.Fatalf("begin operation: claimed=%v operation=%#v err=%v", claimed, operation, err)
	}
	if err := st.FinishFinanceOperation(ctx, operation.ID, FinanceOperationFailed, "", json.RawMessage(`{"retryable":true}`), errors.New("provider timeout"), nil); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetFinanceOperation(ctx, operation.IdempotencyKey)
	if err != nil || stored.Status != FinanceOperationFailed || stored.AttemptCount != 1 || stored.LastError != "provider timeout" {
		t.Fatalf("failed operation state: %#v err=%v", stored, err)
	}
	if claim, err := st.BeginFinanceOperation(ctx, &FinanceOperation{
		IdempotencyKey: operation.IdempotencyKey, OperationType: operation.OperationType,
		TargetType: operation.TargetType, TargetID: operation.TargetID, Provider: "stripe", Request: operation.Request,
	}); err != nil || !claim {
		t.Fatalf("retry operation: claimed=%v err=%v", claim, err)
	}
	if err := st.FinishFinanceOperation(ctx, operation.ID, FinanceOperationReconciliationRequired, "re_123", json.RawMessage(`{"status":"pending"}`), nil, ptrTime(time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListFinanceOperationEvents(ctx, operation.ID)
	if err != nil || len(events) != 4 {
		t.Fatalf("expected started/failed/started/reconcile event history; got %#v err=%v", events, err)
	}
	if events[1].Error != "provider timeout" || events[3].Status != FinanceOperationReconciliationRequired {
		t.Fatalf("operation history lost failure/reconciliation details: %#v", events)
	}
	if _, err := st.BeginFinanceOperation(ctx, &FinanceOperation{
		IdempotencyKey: operation.IdempotencyKey, OperationType: operation.OperationType,
		TargetType: operation.TargetType, TargetID: operation.TargetID, Provider: "stripe", Request: json.RawMessage(`{"amount_minor":251}`),
	}); !errors.Is(err, ErrFinanceIdempotencyConflict) {
		t.Fatalf("operation idempotency key reused with different payload: %v", err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
