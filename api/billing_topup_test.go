package api

import (
	"context"
	"testing"
	"time"

	"account/internal/store"
)

func topupHandler(t *testing.T) (*handler, store.Store, *store.User) {
	t.Helper()
	st := store.NewMemoryStore()
	h := &handler{store: st}

	user := &store.User{
		ID: "payer-1", Name: "payer", Email: "payer@example.com",
		EmailVerified: true, Role: store.RoleUser,
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return h, st, user
}

func paidSession(intent string, amountTotal int64) *stripeCheckoutSession {
	return &stripeCheckoutSession{
		ID:            "cs_test_1",
		Mode:          "payment",
		PaymentIntent: intent,
		PaymentStatus: "paid",
		AmountTotal:   amountTotal,
		Currency:      "cny",
		Metadata:      map[string]string{"kind": "paygo"},
	}
}

func TestCreditTopUpAddsBalanceAndLedger(t *testing.T) {
	h, st, user := topupHandler(t)
	ctx := context.Background()

	// ¥50.00 arrives from Stripe as 5000 minor units.
	if err := h.creditTopUpBalance(ctx, user.ID, paidSession("pi_abc", 5000)); err != nil {
		t.Fatalf("credit top-up: %v", err)
	}

	state, err := st.GetAccountQuotaState(ctx, user.ID)
	if err != nil || state == nil {
		t.Fatalf("expected quota state, err=%v", err)
	}
	if state.CurrentBalance != 50.0 {
		t.Fatalf("expected balance 50.00 from amount_total 5000, got %v", state.CurrentBalance)
	}

	ledger, err := st.ListBillingLedgerByAccount(ctx, user.ID, 10)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("expected one ledger entry, got %d", len(ledger))
	}
	if ledger[0].EntryType != topUpEntryType {
		t.Fatalf("unexpected entry type %q", ledger[0].EntryType)
	}
	if ledger[0].AmountDelta != 50.0 || ledger[0].BalanceAfter != 50.0 {
		t.Fatalf("ledger does not reconcile with balance: %+v", ledger[0])
	}
}

// Stripe retries deliveries, and the event guard deliberately replays attempts
// that did not reach `processed`. Crediting must therefore be safe to run
// twice for the same payment — otherwise a retry mints money.
func TestCreditTopUpIsIdempotentPerPayment(t *testing.T) {
	h, st, user := topupHandler(t)
	ctx := context.Background()
	session := paidSession("pi_repeat", 10000)

	for i := 0; i < 3; i++ {
		if err := h.creditTopUpBalance(ctx, user.ID, session); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}

	state, _ := st.GetAccountQuotaState(ctx, user.ID)
	if state.CurrentBalance != 100.0 {
		t.Fatalf("three deliveries of one ¥100 payment credited %v", state.CurrentBalance)
	}
	ledger, _ := st.ListBillingLedgerByAccount(ctx, user.ID, 10)
	if len(ledger) != 1 {
		t.Fatalf("expected exactly one ledger entry after retries, got %d", len(ledger))
	}
}

// Distinct payments must each credit, or a customer's second top-up would be
// swallowed by the idempotency guard.
func TestCreditTopUpAccumulatesDistinctPayments(t *testing.T) {
	h, st, user := topupHandler(t)
	ctx := context.Background()

	if err := h.creditTopUpBalance(ctx, user.ID, paidSession("pi_one", 5000)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.creditTopUpBalance(ctx, user.ID, paidSession("pi_two", 10000)); err != nil {
		t.Fatalf("second: %v", err)
	}

	state, _ := st.GetAccountQuotaState(ctx, user.ID)
	if state.CurrentBalance != 150.0 {
		t.Fatalf("expected 50 + 100 = 150, got %v", state.CurrentBalance)
	}
	ledger, _ := st.ListBillingLedgerByAccount(ctx, user.ID, 10)
	if len(ledger) != 2 {
		t.Fatalf("expected two ledger entries, got %d", len(ledger))
	}
}

func TestCreditTopUpSkipsNonPayingSessions(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		session *stripeCheckoutSession
	}{
		{"unpaid", &stripeCheckoutSession{
			ID: "cs_unpaid", PaymentIntent: "pi_unpaid",
			PaymentStatus: "unpaid", AmountTotal: 5000,
		}},
		{"no payment required", &stripeCheckoutSession{
			ID: "cs_free", PaymentIntent: "pi_free",
			PaymentStatus: "no_payment_required", AmountTotal: 5000,
		}},
		{"zero amount", &stripeCheckoutSession{
			ID: "cs_zero", PaymentIntent: "pi_zero",
			PaymentStatus: "paid", AmountTotal: 0,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st, user := topupHandler(t)
			if err := h.creditTopUpBalance(ctx, user.ID, tc.session); err != nil {
				t.Fatalf("expected a clean skip, got %v", err)
			}
			ledger, _ := st.ListBillingLedgerByAccount(ctx, user.ID, 10)
			if len(ledger) != 0 {
				t.Fatalf("expected no ledger entry, got %d", len(ledger))
			}
			if state, err := st.GetAccountQuotaState(ctx, user.ID); err == nil && state != nil && state.CurrentBalance != 0 {
				t.Fatalf("expected balance untouched, got %v", state.CurrentBalance)
			}
		})
	}
}

// Paying settles the arrears episode; leaving the flag set would keep the
// customer throttled after they have paid.
func TestCreditTopUpClearsArrears(t *testing.T) {
	h, st, user := topupHandler(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID:  user.ID,
		Arrears:      true,
		ArrearsSince: &now,
	}); err != nil {
		t.Fatalf("seed arrears: %v", err)
	}

	if err := h.creditTopUpBalance(ctx, user.ID, paidSession("pi_settle", 5000)); err != nil {
		t.Fatalf("credit: %v", err)
	}

	state, _ := st.GetAccountQuotaState(ctx, user.ID)
	if state.Arrears {
		t.Fatal("expected arrears cleared after a successful top-up")
	}
	if state.ArrearsSince != nil {
		t.Fatal("expected arrears_since cleared after a successful top-up")
	}
	if state.CurrentBalance != 50.0 {
		t.Fatalf("expected balance 50, got %v", state.CurrentBalance)
	}
}

// The derived ledger id must be stable: if it ever changed, previously
// credited payments would look uncredited and could be credited again.
func TestTopUpLedgerIDIsDeterministic(t *testing.T) {
	first := topUpLedgerID("pi_stable")
	second := topUpLedgerID("  pi_stable  ")
	if first != second {
		t.Fatalf("expected whitespace-insensitive stability, got %q vs %q", first, second)
	}
	if other := topUpLedgerID("pi_different"); other == first {
		t.Fatal("distinct payments must map to distinct ledger ids")
	}
}
