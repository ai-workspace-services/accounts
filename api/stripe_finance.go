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
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	PaymentIntent any               `json:"payment_intent"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Reason        string            `json:"reason"`
	Created       int64             `json:"created"`
	Metadata      map[string]string `json:"metadata"`
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
	key := "stripe-refund:" + refund.ID
	operationKey := refund.Metadata["finance_operation_key"]
	if operationKey == "" {
		// Metadata-less replays may use a reference already bound by a validated
		// response. Never infer association from amount alone or ignore another
		// pending intent's reservation.
		invoice, err := h.store.GetFinanceInvoice(ctx, payment.InvoiceID)
		if err != nil {
			return err
		}
		if invoice.SubscriptionUUID != "" {
			op, err := h.store.GetFinanceOperation(ctx, "subscription-refund:"+invoice.SubscriptionUUID)
			if err != nil && !errors.Is(err, store.ErrFinanceRecordNotFound) {
				return err
			}
			if err == nil && op.ProviderOperationID == refund.ID {
				operationKey = op.IdempotencyKey
				copy := *refund
				copy.Metadata = map[string]string{"finance_operation_key": operationKey}
				refund = &copy
			}
		}
	}
	if operationKey != "" {
		op, err := h.store.GetFinanceOperation(ctx, operationKey)
		if err != nil {
			return err
		}
		if _, err := h.validateOperationRefund(ctx, op, refund); err != nil {
			return err
		}
		key = operationKey
	}
	_, err = h.store.RecordFinanceRefund(ctx, &store.FinanceRefund{
		IdempotencyKey: key,
		PaymentID:      payment.ID, Provider: "stripe", ProviderRefundID: refund.ID,
		AmountMinor: refund.Amount, Currency: refund.Currency, Reason: refund.Reason,
		RefundedAt: time.Unix(refund.Created, 0).UTC(),
	})
	if err != nil {
		return err
	}
	invoice, err := h.store.GetFinanceInvoice(ctx, payment.InvoiceID)
	if err != nil {
		return err
	}
	// Preserve the existing campaign risk policy for Clover refund snapshots,
	// which no longer depend on Charge.invoice being present. A successful
	// partial refund holds/reverses rewards while retaining subscription access.
	if invoice.ProviderInvoiceID != "" {
		if err := h.store.RecordReferralRisk(ctx, store.ReferralPaymentRisk{
			SourceKey: "stripe-refund:" + refund.ID, ProviderInvoiceID: invoice.ProviderInvoiceID,
			Type: "refund", Reason: "Stripe settled refund",
			OccurredAt: time.Unix(refund.Created, 0).UTC(),
		}); err != nil {
			return err
		}
	}
	// The self-service runner owns provider cancellation and local access work.
	// A racing webhook may materialize the fact but cannot complete that intent.
	if operationKey != "" {
		return nil
	}
	// Modern Charge snapshots may omit the legacy invoice reference. Resolve
	// full-refund access through our immutable payment -> invoice -> subscription
	// keys, so revocation does not depend on that removed provider field.
	facts, err := h.store.ListFinanceRefunds(ctx, payment.AccountUUID, 0)
	if err != nil {
		return err
	}
	var refunded int64
	for _, fact := range facts {
		if fact.PaymentID == payment.ID {
			refunded += fact.AmountMinor
		}
	}
	if refunded != payment.AmountMinor {
		return nil
	}
	if invoice.SubscriptionUUID == "" {
		return nil
	}
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, payment.AccountUUID)
	if err != nil {
		return err
	}
	for _, subscription := range subscriptions {
		if subscription.ID == invoice.SubscriptionUUID && subscription.Provider == "stripe" {
			settled, _, err := h.refundReviewSettlement(ctx, refundReviewRequest{AccountID: payment.AccountUUID, SubscriptionID: subscription.ID})
			if err != nil || !settled {
				return err
			}
			if err := h.revokeRefundedSubscription(ctx, payment.AccountUUID, subscription.ExternalID); err != nil {
				return err
			}
			return h.completeSettledRefundReview(ctx, payment.AccountUUID, subscription.ID)
		}
	}
	return errors.New("refunded invoice has no matching local subscription")
}

// reconcileProcessedStripeRefund repairs the access postcondition for a
// verified full refund after a prior webhook attempt was already acknowledged.
// It requires a complete local refund ledger, a matching local
// payment-to-subscription link, and Stripe's current subscription still
// pointing at that refunded invoice before changing access.
func (h *handler) reconcileProcessedStripeRefund(ctx context.Context, event stripeEvent) error {
	if h.stripe == nil || !h.stripe.enabled() {
		return nil
	}
	var paymentIntentID string
	switch event.Type {
	case "refund.created", "refund.updated":
		var refund stripeRefund
		if err := json.Unmarshal(event.Data.Object, &refund); err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(refund.Status), "succeeded") || refund.Amount <= 0 {
			return nil
		}
		paymentIntentID = customerIDFromAny(refund.PaymentIntent)
	case "charge.refunded":
		var charge stripeCharge
		if err := json.Unmarshal(event.Data.Object, &charge); err != nil {
			return err
		}
		if !charge.Refunded || charge.Amount <= 0 || charge.AmountRefunded < charge.Amount {
			return nil
		}
		for _, refund := range charge.Refunds.Data {
			if !strings.EqualFold(strings.TrimSpace(refund.Status), "succeeded") {
				return nil
			}
		}
		paymentIntentID = customerIDFromAny(charge.PaymentIntent)
	default:
		return nil
	}
	if paymentIntentID == "" {
		return nil
	}
	payment, err := h.store.GetFinancePaymentByProvider(ctx, "stripe", paymentIntentID)
	if errors.Is(err, store.ErrFinanceRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	invoice, err := h.store.GetFinanceInvoice(ctx, payment.InvoiceID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(invoice.Provider, "stripe") || invoice.AccountUUID != payment.AccountUUID || invoice.SubscriptionUUID == "" || invoice.ProviderInvoiceID == "" {
		return nil
	}
	refunds, err := h.store.ListFinanceRefunds(ctx, payment.AccountUUID, 0)
	if err != nil {
		return err
	}
	var refunded int64
	for _, refund := range refunds {
		if refund.PaymentID != payment.ID {
			continue
		}
		if !strings.EqualFold(refund.Currency, payment.Currency) || refund.AmountMinor <= 0 || refund.AmountMinor > payment.AmountMinor-refunded {
			return store.ErrFinanceIdempotencyConflict
		}
		refunded += refund.AmountMinor
	}
	if refunded != payment.AmountMinor {
		return nil
	}
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, payment.AccountUUID)
	if err != nil {
		return err
	}
	var local *store.Subscription
	for i := range subscriptions {
		if subscriptions[i].ID == invoice.SubscriptionUUID && strings.EqualFold(subscriptions[i].Provider, "stripe") && strings.EqualFold(subscriptions[i].Kind, "subscription") {
			local = &subscriptions[i]
			break
		}
	}
	if local == nil || strings.TrimSpace(local.ExternalID) == "" {
		return nil
	}
	settled, _, err := h.refundReviewSettlement(ctx, refundReviewRequest{AccountID: payment.AccountUUID, SubscriptionID: local.ID})
	if err != nil || !settled {
		return err
	}
	remote, err := h.stripe.fetchSubscription(ctx, local.ExternalID)
	if err != nil {
		return err
	}
	if remote.ID != local.ExternalID || strings.TrimSpace(remote.Metadata["user_id"]) != payment.AccountUUID || customerIDFromAny(remote.LatestInvoice) != invoice.ProviderInvoiceID {
		return nil
	}
	wasReconciled := strings.EqualFold(local.Status, "refunded")
	if err := h.revokeRefundedSubscription(ctx, payment.AccountUUID, local.ExternalID); err != nil {
		return err
	}
	profile, profileErr := h.store.GetAccountBillingProfile(ctx, payment.AccountUUID)
	if profileErr != nil && !errors.Is(profileErr, store.ErrUserNotFound) {
		return profileErr
	}
	if !wasReconciled || profile == nil || !strings.EqualFold(profile.PricingRuleVersion, "plan:"+store.BillingPlanFree) {
		h.publishBillingEvent(ctx, &store.BillingEvent{Type: "subscription_refunded", UserID: payment.AccountUUID, PlanID: local.PlanID, ExternalID: local.ExternalID})
	}
	return nil
}

// A paid invoice remains "paid" at Stripe after a refund. Do not let a late
// subscription/invoice event turn that old payment back into paid access.
func (h *handler) reconcileRefundedLatestInvoice(ctx context.Context, source *stripeSubscription) (bool, error) {
	if source == nil || source.Metadata["user_id"] == "" || customerIDFromAny(source.LatestInvoice) == "" {
		return false, nil
	}
	userID := source.Metadata["user_id"]
	payments, err := h.store.ListFinancePayments(ctx, userID, 0)
	if err != nil {
		return false, err
	}
	refunds, err := h.store.ListFinanceRefunds(ctx, userID, 0)
	if err != nil {
		return false, err
	}
	count := 0
	for _, payment := range payments {
		if payment.Provider != "stripe" {
			continue
		}
		invoice, err := h.store.GetFinanceInvoice(ctx, payment.InvoiceID)
		if err != nil {
			return false, err
		}
		if invoice.AccountUUID != userID || invoice.ProviderInvoiceID != customerIDFromAny(source.LatestInvoice) {
			continue
		}
		if payment.AmountMinor <= 0 {
			continue
		}
		count++
		var total int64
		for _, refund := range refunds {
			if refund.PaymentID == payment.ID {
				total += refund.AmountMinor
			}
		}
		if total != payment.AmountMinor {
			return false, nil
		}
	}
	if count == 0 {
		return false, nil
	}
	return true, h.revokeRefundedSubscription(ctx, userID, source.ID)
}
