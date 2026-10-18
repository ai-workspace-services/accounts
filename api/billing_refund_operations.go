package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

// Identity is local: provider references are attributes of the pinned payment.
type subscriptionRefundIntent struct {
	PaymentID       string `json:"payment_uuid"`
	AccountID       string `json:"account_uuid"`
	SubscriptionID  string `json:"subscription_uuid"`
	ExternalID      string `json:"subscription_external_id"`
	PaymentIntentID string `json:"payment_intent_id"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
}

func subscriptionRefundKey(sub *store.Subscription) string { return "subscription-refund:" + sub.ID }

func (h *handler) respondSubscriptionRefund(c *gin.Context, err error) {
	if err != nil {
		code := http.StatusAccepted
		if errors.Is(err, store.ErrFinanceRefundExceedsPayment) || errors.Is(err, store.ErrFinanceIdempotencyConflict) {
			code = http.StatusConflict
		}
		respondError(c, code, "refund_reconcile_needed", "refund has not completed; retry this subscription request to reconcile")
		return
	}
	c.JSON(http.StatusOK, gin.H{"refunded": true})
}

func (h *handler) resumeSubscriptionRefund(ctx context.Context, accountID string, sub *store.Subscription) (bool, error) {
	op, err := h.store.GetFinanceOperation(ctx, subscriptionRefundKey(sub))
	if errors.Is(err, store.ErrFinanceRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var intent subscriptionRefundIntent
	if err := json.Unmarshal(op.Request, &intent); err != nil || intent.AccountID != accountID || intent.SubscriptionID != sub.ID || intent.ExternalID != sub.ExternalID {
		return true, store.ErrFinanceIdempotencyConflict
	}
	return true, h.runSubscriptionRefund(ctx, op)
}

func (h *handler) startSubscriptionRefund(ctx context.Context, payment *store.FinancePayment, sub *store.Subscription) error {
	request, err := json.Marshal(subscriptionRefundIntent{payment.ID, payment.AccountUUID, sub.ID, sub.ExternalID, payment.ProviderPaymentID, payment.AmountMinor, payment.Currency})
	if err != nil {
		return err
	}
	return h.runSubscriptionRefund(ctx, &store.FinanceOperation{IdempotencyKey: subscriptionRefundKey(sub), OperationType: "refund", TargetType: "payment", TargetID: payment.ID, Provider: "stripe", Request: request})
}

// A metadata association is accepted only after checking immutable local facts.
// Both response and webhook take this path; neither trusts provider metadata as
// proof of ownership or as a primary key for a payment.
func (h *handler) validateOperationRefund(ctx context.Context, op *store.FinanceOperation, refund *stripeRefund) (*store.FinancePayment, error) {
	intent, payment, err := h.loadSubscriptionRefundIntent(ctx, op)
	if err != nil {
		return nil, err
	}
	if refund.Metadata["finance_operation_key"] != op.IdempotencyKey || !strings.HasPrefix(refund.ID, "re_") || refund.Created <= 0 || refund.Amount != intent.AmountMinor || !strings.EqualFold(refund.Currency, intent.Currency) || customerIDFromAny(refund.PaymentIntent) != intent.PaymentIntentID || op.ProviderOperationID != "" && op.ProviderOperationID != refund.ID {
		return nil, store.ErrFinanceIdempotencyConflict
	}
	return payment, nil
}

func (h *handler) loadSubscriptionRefundIntent(ctx context.Context, op *store.FinanceOperation) (subscriptionRefundIntent, *store.FinancePayment, error) {
	var intent subscriptionRefundIntent
	if json.Unmarshal(op.Request, &intent) != nil || op.OperationType != "refund" || op.TargetType != "payment" || op.Provider != "stripe" || op.TargetID != intent.PaymentID || intent.AccountID == "" || intent.SubscriptionID == "" || intent.AmountMinor <= 0 {
		return intent, nil, store.ErrFinanceIdempotencyConflict
	}
	payment, err := h.store.GetFinancePaymentByProvider(ctx, "stripe", intent.PaymentIntentID)
	if err != nil {
		return intent, nil, err
	}
	if payment.ID != op.TargetID || payment.Provider != op.Provider || payment.AccountUUID != intent.AccountID || payment.AmountMinor != intent.AmountMinor || payment.Currency != intent.Currency {
		return intent, nil, store.ErrFinanceIdempotencyConflict
	}
	invoice, err := h.store.GetFinanceInvoice(ctx, payment.InvoiceID)
	if err != nil {
		return intent, nil, err
	}
	if invoice.AccountUUID != intent.AccountID || invoice.SubscriptionUUID != intent.SubscriptionID || invoice.Provider != "stripe" || invoice.AmountMinor != payment.AmountMinor || invoice.Currency != payment.Currency {
		return intent, nil, store.ErrFinanceIdempotencyConflict
	}
	subs, err := h.store.ListSubscriptionsByUser(ctx, intent.AccountID)
	if err != nil {
		return intent, nil, err
	}
	for _, sub := range subs {
		if sub.ID == intent.SubscriptionID && sub.ExternalID == intent.ExternalID && sub.Provider == "stripe" && sub.Kind == "subscription" && op.IdempotencyKey == subscriptionRefundKey(&sub) {
			return intent, payment, nil
		}
	}
	return intent, nil, store.ErrFinanceIdempotencyConflict
}

// Query all pages before any retry. An empty query after an ambiguous call is
// NOT evidence that a new POST is safe (Stripe can age out idempotency keys).
func (c *stripeClient) findOperationRefund(ctx context.Context, op *store.FinanceOperation, intent subscriptionRefundIntent) (*stripeRefund, error) {
	if op.ProviderOperationID != "" {
		var refund stripeRefund
		if err := c.doJSON(ctx, http.MethodGet, "/refunds/"+url.PathEscape(op.ProviderOperationID), &refund); err != nil {
			return nil, err
		}
		return &refund, nil
	}
	query := url.Values{"payment_intent": {intent.PaymentIntentID}, "limit": {"100"}}
	var found *stripeRefund
	for page := 0; page < 100; page++ {
		var result struct {
			Data    []stripeRefund `json:"data"`
			HasMore bool           `json:"has_more"`
		}
		if err := c.doJSON(ctx, http.MethodGet, "/refunds?"+query.Encode(), &result); err != nil {
			return nil, err
		}
		for _, refund := range result.Data {
			if refund.Metadata["finance_operation_key"] != op.IdempotencyKey || found != nil {
				return nil, errors.New("payment has an unassociated or duplicate provider refund; reconciliation required")
			}
			copy := refund
			found = &copy
		}
		if !result.HasMore {
			return found, nil
		}
		if len(result.Data) == 0 {
			break
		}
		query.Set("starting_after", result.Data[len(result.Data)-1].ID)
	}
	return nil, errors.New("incomplete provider refund history")
}

func (h *handler) runSubscriptionRefund(parent context.Context, op *store.FinanceOperation) (resultErr error) {
	// Finish persists even if the browser disconnects. Provider work is bounded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 45*time.Second)
	defer cancel()
	claimed, err := h.store.BeginFinanceOperation(ctx, op)
	if err != nil || !claimed {
		return err
	}
	var refund *stripeRefund
	// A saved pre-provider failure is safe to retry. Missing state after a
	// crash is ambiguous, and therefore only permits provider queries.
	mayHaveSubmitted := op.AttemptCount > 1
	var previous struct {
		ProviderRequestStarted *bool `json:"provider_request_started"`
	}
	if json.Unmarshal(op.Response, &previous) == nil && previous.ProviderRequestStarted != nil {
		mayHaveSubmitted = *previous.ProviderRequestStarted
	}
	defer func() {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer finishCancel()
		status, providerID := store.FinanceOperationSucceeded, ""
		if resultErr != nil {
			status = store.FinanceOperationReconciliationRequired
		}
		if refund != nil {
			providerID = refund.ID
		}
		response, _ := json.Marshal(map[string]any{"provider_request_started": mayHaveSubmitted, "refund": refund})
		if err := h.store.FinishFinanceOperation(finishCtx, op.ID, status, providerID, response, resultErr, nil, op.AttemptCount); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	intent, _, err := h.loadSubscriptionRefundIntent(ctx, op)
	if err != nil {
		return err
	}
	refund, err = h.stripe.findOperationRefund(ctx, op, intent)
	if err != nil {
		return err
	}
	if refund == nil {
		if mayHaveSubmitted {
			return errors.New("previous refund outcome unknown; provider query found no matching refund")
		}
		mayHaveSubmitted = true
		if err := h.store.CheckpointFinanceOperation(ctx, op.ID, op.AttemptCount, json.RawMessage(`{"provider_request_started":true}`)); err != nil {
			return err
		}
		refund, err = h.stripe.refundPaymentIntent(ctx, intent.PaymentIntentID, op.IdempotencyKey, intent.AmountMinor)
		if err != nil {
			return err
		}
	}
	mayHaveSubmitted = true
	if _, err := h.validateOperationRefund(ctx, op, refund); err != nil {
		// Never pin an unvalidated provider reference in Finish.
		refund = nil
		return err
	}
	if refund.Status != "succeeded" {
		return fmt.Errorf("provider refund is %s", refund.Status)
	}
	if err := h.recordStripeRefund(ctx, refund); err != nil {
		return err
	}
	remote, err := h.stripe.fetchSubscription(ctx, intent.ExternalID)
	if err != nil {
		return err
	}
	if remote.ID != intent.ExternalID {
		return store.ErrFinanceIdempotencyConflict
	}
	// Cancellation may also have succeeded before a lost response. Query its
	// current state rather than depending on DELETE accepting a second cancel.
	if remote.Status != "canceled" {
		if err := h.stripe.cancelSubscription(ctx, intent.ExternalID); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return h.revokeRefundedSubscription(ctx, intent.AccountID, intent.ExternalID)
}
