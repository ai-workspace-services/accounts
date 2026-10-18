package api

import (
	"context"
	"testing"
	"time"

	"account/internal/store"
)

func TestRecordStripePaidInvoicePersistsInvoiceAndPaymentIdempotently(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Stripe Test", Email: "stripe-finance@example.com", EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{ID: "local-subscription", UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_test_123", Status: "active"}
	if err := st.UpsertSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	invoice := &stripeInvoice{
		ID: "in_test_paid_123", Subscription: "sub_test_123", Status: "paid",
		PaymentIntent: "pi_test_paid_123", AmountPaid: 1200, Currency: "usd", Created: paidAt.Add(-time.Hour).Unix(),
	}
	invoice.StatusTransitions.PaidAt = paidAt.Unix()
	h := &handler{store: st}
	for range 2 {
		if err := h.recordStripePaidInvoice(ctx, invoice, &stripeSubscription{ID: "sub_test_123"}, user.ID); err != nil {
			t.Fatalf("record invoice: %v", err)
		}
	}
	invoices, err := st.ListFinanceInvoices(ctx, user.ID, subscription.ID, 10)
	if err != nil || len(invoices) != 1 {
		t.Fatalf("want one local invoice, got %d (err=%v)", len(invoices), err)
	}
	payments, err := st.ListFinancePayments(ctx, user.ID, 10)
	if err != nil || len(payments) != 1 {
		t.Fatalf("want one local payment, got %d (err=%v)", len(payments), err)
	}
	if invoices[0].ProviderInvoiceID != invoice.ID || invoices[0].AmountMinor != 1200 || invoices[0].Currency != "USD" {
		t.Fatalf("unexpected invoice facts: %+v", invoices[0])
	}
	if payments[0].ProviderPaymentID != "pi_test_paid_123" || payments[0].AmountMinor != 1200 || payments[0].Currency != "USD" {
		t.Fatalf("unexpected payment facts: %+v", payments[0])
	}
}

func TestRecordStripePaidInvoiceRejectsMismatchedSubscriptionAndInvalidCurrency(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Stripe Test", Email: "stripe-finance-reject@example.com", EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{ID: "local-subscription", UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_expected", Status: "active"}
	if err := st.UpsertSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Now().UTC().Unix()
	h := &handler{store: st}
	base := &stripeInvoice{ID: "in_rejected", Subscription: "sub_other", Status: "paid", PaymentIntent: "pi_rejected", AmountPaid: 500, Currency: "USD"}
	base.StatusTransitions.PaidAt = paidAt
	if err := h.recordStripePaidInvoice(ctx, base, &stripeSubscription{ID: "sub_expected"}, user.ID); err == nil {
		t.Fatal("expected mismatched subscription to be rejected")
	}
	base.Subscription = "sub_expected"
	base.Currency = "U$D"
	if err := h.recordStripePaidInvoice(ctx, base, &stripeSubscription{ID: "sub_expected"}, user.ID); err == nil {
		t.Fatal("expected invalid currency to be rejected")
	}
	invoices, err := st.ListFinanceInvoices(ctx, user.ID, "", 10)
	if err != nil || len(invoices) != 0 {
		t.Fatalf("invalid invoices must have no local side effects: %d (err=%v)", len(invoices), err)
	}
}

func TestRecordStripePaidInvoiceDoesNotInventPaymentIntent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Stripe Test", Email: "stripe-finance-no-pi@example.com", EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{ID: "local-subscription", UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_no_pi", Status: "active"}
	if err := st.UpsertSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	invoice := &stripeInvoice{ID: "in_no_pi", Subscription: "sub_no_pi", Status: "paid", AmountPaid: 100, Currency: "USD"}
	invoice.StatusTransitions.PaidAt = time.Now().UTC().Unix()
	if err := (&handler{store: st}).recordStripePaidInvoice(ctx, invoice, &stripeSubscription{ID: "sub_no_pi"}, user.ID); err == nil {
		t.Fatal("expected paid invoice without payment intent to be left for repair")
	}
	invoices, err := st.ListFinanceInvoices(ctx, user.ID, "", 10)
	if err != nil || len(invoices) != 1 {
		t.Fatalf("invoice fact should be durable for retry: %d (err=%v)", len(invoices), err)
	}
	payments, err := st.ListFinancePayments(ctx, user.ID, 10)
	if err != nil || len(payments) != 0 {
		t.Fatalf("must not invent a payment: %+v (err=%v)", payments, err)
	}
}
