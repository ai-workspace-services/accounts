package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"account/internal/store"
)

func TestSandboxRejectsSignedLiveModeEvent(t *testing.T) {
	router, _, _ := newBillingWebhookHarness(t)
	rec := postStripeEvent(t, router, []byte(`{"id":"evt_live_rejected","type":"invoice.paid","livemode":true,"data":{"object":{}}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sandbox accepted live event: %d", rec.Code)
	}
}

func TestCloverInvoiceAndSubscriptionSnapshots(t *testing.T) {
	var invoice stripeInvoice
	err := json.Unmarshal([]byte(`{"id":"in_clover","status":"paid","amount_paid":1200,"currency":"usd","parent":{"subscription_details":{"subscription":"sub_clover"}},"payments":{"data":[{"status":"paid","amount_paid":1200,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_clover"}}],"has_more":false}}`), &invoice)
	if err != nil || customerIDFromAny(invoice.Subscription) != "sub_clover" || customerIDFromAny(invoice.PaymentIntent) != "pi_clover" {
		t.Fatalf("Clover invoice lost provider references: %#v, %v", invoice, err)
	}
	var sub stripeSubscription
	err = json.Unmarshal([]byte(`{"id":"sub_clover","items":{"data":[{"current_period_start":1770000000,"current_period_end":1772600000,"price":{"id":"price_clover"}}]}}`), &sub)
	if err != nil || sub.CurrentPeriodStart != 1770000000 || sub.CurrentPeriodEnd != 1772600000 {
		t.Fatalf("Clover item period lost: %#v, %v", sub, err)
	}
	for _, body := range []string{
		`{"amount_paid":1200,"currency":"usd","payments":{"data":[{"status":"open","amount_paid":1200,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_not_paid"}}]}}`,
		`{"amount_paid":1200,"currency":"usd","payments":{"has_more":true,"data":[{"status":"paid","amount_paid":1200,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_incomplete"}}]}}`,
		`{"amount_paid":1200,"currency":"usd","payments":{"data":[{"status":"paid","amount_paid":1000,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_partial"}}]}}`,
	} {
		if err := json.Unmarshal([]byte(body), &invoice); err != nil || customerIDFromAny(invoice.PaymentIntent) != "" {
			t.Fatalf("must not invent a settled payment from incomplete facts: %#v, %v", invoice, err)
		}
	}
}

func TestStripeRefundFactsAreIdempotentAndBoundedByOriginalPayment(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Sandbox", Email: "p0f-refund@example.invalid"}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	invoice := &store.FinanceInvoice{IdempotencyKey: "refund-test-invoice", AccountUUID: user.ID, Provider: "stripe", ProviderInvoiceID: "in_refund", AmountMinor: 1200, Currency: "USD"}
	if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	payment := &store.FinancePayment{IdempotencyKey: "refund-test-payment", InvoiceID: invoice.ID, AccountUUID: user.ID, Provider: "stripe", ProviderPaymentID: "pi_refund", AmountMinor: 1200, Currency: "USD"}
	if _, err := st.RecordFinancePayment(ctx, payment); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	refund := stripeRefund{ID: "re_partial", Status: "pending", PaymentIntent: "pi_refund", Amount: 400, Currency: "usd", Created: 1770000000}
	if err := h.recordStripeRefund(ctx, &refund); err != nil {
		t.Fatal(err)
	}
	facts, _ := st.ListFinanceRefunds(ctx, user.ID, 0)
	if len(facts) != 0 {
		t.Fatal("pending refund must not post")
	}
	refund.Status = "succeeded"
	for _, eventType := range []string{"refund.created", "refund.updated", "refund.updated"} {
		body, _ := json.Marshal(refund)
		event := stripeEvent{Type: eventType}
		event.Data.Object = body
		if err := h.handleStripeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	facts, _ = st.ListFinanceRefunds(ctx, user.ID, 0)
	if len(facts) != 1 || facts[0].AmountMinor != 400 || facts[0].PaymentID != payment.ID {
		t.Fatalf("refund facts: %#v", facts)
	}
	refund.ID, refund.Amount = "re_over", 801
	if err := h.recordStripeRefund(ctx, &refund); !errors.Is(err, store.ErrFinanceRefundExceedsPayment) {
		t.Fatalf("over refund must fail: %v", err)
	}
	refund.ID, refund.Amount, refund.Currency = "re_wrong_currency", 100, "EUR"
	if err := h.recordStripeRefund(ctx, &refund); err == nil {
		t.Fatal("currency mismatch must fail")
	}
	refund.ID, refund.PaymentIntent, refund.Currency = "re_out_of_order", "pi_not_yet_seen", "USD"
	if err := h.recordStripeRefund(ctx, &refund); !errors.Is(err, store.ErrFinanceRecordNotFound) {
		t.Fatalf("out-of-order refund must remain retryable: %v", err)
	}
}

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
