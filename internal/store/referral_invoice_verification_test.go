package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReferralVerifiedInvoiceDoesNotRewriteSettledFacts(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{Name: "verified-owner", Email: "verified-owner@example.invalid"}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	sub := &Subscription{UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: "verification-sub", Status: "active"}
	if err := st.UpsertSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	subs, err := st.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil || len(subs) != 1 {
		t.Fatal("subscription fixture missing")
	}
	subID := subs[0].ID
	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	invoice := &FinanceInvoice{IdempotencyKey: "verification-invoice", AccountUUID: user.ID, SubscriptionUUID: subID, Provider: "stripe", ProviderInvoiceID: "verification-provider-invoice", AmountMinor: 1000, Currency: "USD", Description: "Settled Stripe subscription invoice", IssuedAt: now.Add(-2 * time.Hour), ServicePeriodStart: &start, ServicePeriodEnd: &end}
	if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordFinancePayment(ctx, &FinancePayment{IdempotencyKey: "verification-payment", InvoiceID: invoice.ID, AccountUUID: user.ID, Provider: "stripe", ProviderPaymentID: "verification-pi", AmountMinor: 1000, Currency: "USD", PaidAt: now}); err != nil {
		t.Fatal(err)
	}
	if actual, err := st.ReferralExtensionEnd(ctx, subID); err != nil || !actual.IsZero() {
		t.Fatalf("unverified generic fact admitted=%v %v", actual, err)
	}
	proof := &ReferralInvoiceVerification{InvoiceID: invoice.ID, ProviderPriceID: "price-proof", PlanID: "PRO-MONTHLY", ProductSlug: "xconnect"}
	for i := 0; i < 2; i++ {
		if err := st.VerifyReferralInvoice(ctx, proof); err != nil {
			t.Fatal(err)
		}
	}
	if actual, err := st.ReferralExtensionEnd(ctx, subID); err != nil || !actual.Equal(end) {
		t.Fatalf("verified paid fact missing=%v %v", actual, err)
	}
	original, err := st.GetFinanceInvoice(ctx, invoice.ID)
	if err != nil || original.Description != invoice.Description || !original.IssuedAt.Equal(invoice.IssuedAt) {
		t.Fatal("settled invoice rewritten")
	}
	different := *proof
	different.ProviderPriceID = "price-other"
	if err := st.VerifyReferralInvoice(ctx, &different); !errors.Is(err, ErrFinanceIdempotencyConflict) {
		t.Fatalf("conflicting evidence=%v", err)
	}
	invalid := *proof
	invalid.ProductSlug = "knowledge"
	invalid.PlanID = "KNOWLEDGE-MONTHLY"
	if err := st.VerifyReferralInvoice(ctx, &invalid); err == nil {
		t.Fatal("non-XConnect proof accepted")
	}
}
