package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"account/internal/store"
)

// Normalize the Clover invoice shape while continuing to read historical
// webhook snapshots. Ambiguous/multiple payments never become invented facts.
func (invoice *stripeInvoice) UnmarshalJSON(data []byte) error {
	type invoiceJSON stripeInvoice
	var decoded invoiceJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*invoice = stripeInvoice(decoded)
	parent := customerIDFromAny(invoice.Parent.SubscriptionDetails.Subscription)
	legacy := customerIDFromAny(invoice.Subscription)
	if parent != "" && legacy != "" && parent != legacy {
		return errors.New("conflicting Stripe invoice subscription references")
	}
	if legacy == "" {
		invoice.Subscription = parent
	}
	if customerIDFromAny(invoice.PaymentIntent) == "" && !invoice.Payments.HasMore && len(invoice.Payments.Data) == 1 {
		payment := invoice.Payments.Data[0]
		if payment.Status == "paid" && payment.Payment.Type == "payment_intent" && payment.AmountPaid == invoice.AmountPaid && strings.EqualFold(payment.Currency, invoice.Currency) {
			invoice.PaymentIntent = payment.Payment.PaymentIntent
		}
	}
	return nil
}

func (subscription *stripeSubscription) UnmarshalJSON(data []byte) error {
	type subscriptionJSON stripeSubscription
	var decoded subscriptionJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*subscription = stripeSubscription(decoded)
	if subscription.CurrentPeriodStart == 0 && subscription.CurrentPeriodEnd == 0 && len(subscription.Items.Data) == 1 {
		subscription.CurrentPeriodStart = subscription.Items.Data[0].CurrentPeriodStart
		subscription.CurrentPeriodEnd = subscription.Items.Data[0].CurrentPeriodEnd
	}
	return nil
}

type stripeRefund struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PaymentIntent any    `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Reason        string `json:"reason"`
	Created       int64  `json:"created"`
}

// Provider refund IDs, rather than event IDs, make different deliveries and
// created/updated events converge on the same append-only refund fact.
func (h *handler) recordStripeRefund(ctx context.Context, refund *stripeRefund) error {
	if refund.Status != "succeeded" {
		return nil
	}
	intentID := customerIDFromAny(refund.PaymentIntent)
	if !strings.HasPrefix(refund.ID, "re_") || !strings.HasPrefix(intentID, "pi_") || refund.Amount <= 0 || refund.Created <= 0 {
		return errors.New("Stripe settled refund has incomplete payment facts")
	}
	payment, err := h.store.GetFinancePaymentByProvider(ctx, "stripe", intentID)
	if err != nil {
		return err
	} // Retried when the payment webhook arrives later.
	_, err = h.store.RecordFinanceRefund(ctx, &store.FinanceRefund{
		IdempotencyKey: "stripe-refund:" + refund.ID,
		PaymentID:      payment.ID, Provider: "stripe", ProviderRefundID: refund.ID,
		AmountMinor: refund.Amount, Currency: refund.Currency, Reason: refund.Reason,
		RefundedAt: time.Unix(refund.Created, 0).UTC(),
	})
	return err
}
