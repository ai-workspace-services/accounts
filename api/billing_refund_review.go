package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

type refundReviewPayment struct {
	PaymentID         string    `json:"paymentId"`
	InvoiceID         string    `json:"invoiceId"`
	ProviderPaymentID string    `json:"providerPaymentId"`
	Currency          string    `json:"currency"`
	PaidMinor         int64     `json:"paidMinor"`
	RefundedMinor     int64     `json:"refundedMinor"`
	PaidAt            time.Time `json:"paidAt"`
}

type refundReviewRequest struct {
	AccountID      string                `json:"accountId"`
	SubscriptionID string                `json:"subscriptionId"`
	ExternalID     string                `json:"externalId"`
	RequestedAt    time.Time             `json:"requestedAt"`
	FirstPaidAt    *time.Time            `json:"firstPaidAt,omitempty"`
	Payments       []refundReviewPayment `json:"payments"`
	Investigation  string                `json:"investigation,omitempty"`
}

func refundReviewKey(userID string) string { return "subscription-refund-review:" + userID }

func refundWithinWindow(paidAt, requestedAt time.Time) bool {
	return !paidAt.IsZero() && !paidAt.After(requestedAt) && requestedAt.Sub(paidAt) <= refundEligibilityWindow
}

// User requests NEVER call the refund executor, including retries of operations
// created by the retired automatic endpoint. Approval is a separate boundary.
func (h *handler) refundSubscription(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) || !user.MFAEnabled {
		respondError(c, 403, "mfa_required", "MFA is required before requesting a refund")
		return
	}
	var req subscriptionRefundRequest
	if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.ExternalID) == "" {
		respondError(c, 400, "invalid_request", "A subscription is required")
		return
	}
	ctx := c.Request.Context()
	if existing, err := h.store.GetFinanceOperation(ctx, refundReviewKey(user.ID)); err == nil {
		h.respondRefundReview(c, existing)
		return
	} else if !errors.Is(err, store.ErrFinanceRecordNotFound) {
		respondError(c, 503, "refund_status_unavailable", "Cannot verify refund review")
		return
	}
	subs, err := h.store.ListSubscriptionsByUser(ctx, user.ID)
	if err != nil {
		respondError(c, 503, "subscriptions_unavailable", "Cannot verify subscription ownership")
		return
	}
	var owned *store.Subscription
	for i := range subs {
		if subs[i].ExternalID == req.ExternalID && subs[i].Provider == "stripe" && subs[i].Kind == "subscription" {
			owned = &subs[i]
			break
		}
	}
	if owned == nil {
		respondError(c, 404, "subscription_not_found", "Subscription not found")
		return
	}
	intent := refundReviewRequest{AccountID: user.ID, SubscriptionID: owned.ID, ExternalID: owned.ExternalID, RequestedAt: time.Now().UTC(), Payments: []refundReviewPayment{}}
	lifecycle, err := h.store.GetSubscriptionLifecycle(ctx, user.ID)
	if err != nil || lifecycle.OperationID != "" {
		respondError(c, 409, "subscription_operation_pending", "Wait for the current subscription operation")
		return
	}
	if err := h.collectRefundReview(ctx, &intent); err != nil {
		intent.Investigation = "Payment history or subscription-only allocation requires manual verification"
	}
	if intent.FirstPaidAt != nil && intent.Investigation == "" && !refundWithinWindow(*intent.FirstPaidAt, intent.RequestedAt) {
		respondError(c, 422, "refund_window_expired", "The first successful payment is outside the 72-hour application window")
		return
	}
	raw, _ := json.Marshal(intent)
	op := &store.FinanceOperation{IdempotencyKey: refundReviewKey(user.ID), OperationType: "refund_review", TargetType: "account", TargetID: user.ID, Provider: "stripe", Request: raw}
	err = h.store.CreateSubscriptionRefundReview(ctx, op, lifecycle.Version)
	if err != nil {
		if existing, e := h.store.GetFinanceOperation(ctx, refundReviewKey(user.ID)); e == nil {
			h.respondRefundReview(c, existing)
			return
		}
		respondError(c, 503, "refund_request_unavailable", "Could not save refund application")
		return
	}
	h.respondRefundReview(c, op)
}

func (h *handler) respondRefundReview(c *gin.Context, op *store.FinanceOperation) {
	var intent refundReviewRequest
	if json.Unmarshal(op.Request, &intent) != nil || intent.AccountID != op.TargetID || op.OperationType != "refund_review" {
		respondError(c, 503, "refund_review_invalid", "Refund application needs investigation")
		return
	}
	status := "pending_review"
	if intent.Investigation != "" {
		status = "needs_investigation"
	}
	c.JSON(http.StatusAccepted, gin.H{"id": op.ID, "status": status, "requestedAt": intent.RequestedAt, "firstPaidAt": intent.FirstPaidAt, "payments": intent.Payments, "refunded": false})
}

func (h *handler) subscriptionRefundStatus(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	op, err := h.store.GetFinanceOperation(c.Request.Context(), refundReviewKey(user.ID))
	if errors.Is(err, store.ErrFinanceRecordNotFound) {
		c.JSON(200, gin.H{"status": "none"})
		return
	}
	if err != nil {
		respondError(c, 503, "refund_status_unavailable", "Cannot verify refund review")
		return
	}
	h.respondRefundReview(c, op)
}

// Mixed invoices, incomplete history and unrecognized prices require manual
// investigation, never an estimate that might refund PAYG or usage charges.
func (h *handler) collectRefundReview(ctx context.Context, intent *refundReviewRequest) error {
	payments, err := h.store.ListFinancePayments(ctx, intent.AccountID, 0)
	if err != nil {
		return err
	}
	refunds, err := h.store.ListFinanceRefunds(ctx, intent.AccountID, 0)
	if err != nil {
		return err
	}
	if h.stripe == nil || !h.stripe.enabled() {
		return errors.New("provider unavailable")
	}
	remote, err := h.stripe.fetchSubscription(ctx, intent.ExternalID)
	if err != nil {
		return err
	}
	if remote.ID != intent.ExternalID || remote.Metadata["user_id"] != intent.AccountID || customerIDFromAny(remote.Customer) == "" {
		return errors.New("unproven provider ownership")
	}
	invoices, err := h.stripe.listPaidInvoices(ctx, customerIDFromAny(remote.Customer))
	if err != nil {
		return err
	}
	var first time.Time
	var firstSubscription string
	seen := map[string]bool{}
	for _, candidate := range invoices {
		if invoiceSubscriptionID(&candidate) == "" {
			continue
		}
		invoice, e := h.stripe.fetchInvoice(ctx, candidate.ID)
		if e != nil {
			return e
		}
		if invoice.Status != "paid" || invoice.PaidOutOfBand || invoice.AmountPaid <= 0 || invoice.StatusTransitions.PaidAt <= 0 {
			return errors.New("unverified paid history")
		}
		if len(invoice.Lines.Data) == 0 || invoice.Lines.HasMore {
			return errors.New("incomplete invoice allocation")
		}
		for _, line := range invoice.Lines.Data {
			price := customerIDFromAny(line.Price)
			if price == "" {
				price = customerIDFromAny(line.Pricing.PriceDetails.Price)
			}
			plan, e := h.store.GetBillingPlanByPriceID(ctx, price)
			if e != nil || plan.Kind != "subscription" {
				return errors.New("mixed invoice")
			}
		}
		paidAt := time.Unix(invoice.StatusTransitions.PaidAt, 0).UTC()
		if paidAt.After(intent.RequestedAt) {
			return errors.New("future payment")
		}
		if first.IsZero() || paidAt.Before(first) {
			first = paidAt
			firstSubscription = invoiceSubscriptionID(invoice)
		}
		if invoiceSubscriptionID(invoice) != intent.ExternalID {
			continue
		}
		if invoice.BillingReason != "subscription_create" && invoice.BillingReason != "subscription_update" {
			continue
		}
		pi := customerIDFromAny(invoice.PaymentIntent)
		if pi == "" || seen[pi] {
			return errors.New("ambiguous payment")
		}
		seen[pi] = true
		var payment *store.FinancePayment
		for i := range payments {
			if payments[i].Provider == "stripe" && payments[i].ProviderPaymentID == pi {
				payment = &payments[i]
				break
			}
		}
		if payment == nil || payment.AmountMinor != invoice.AmountPaid || !strings.EqualFold(payment.Currency, invoice.Currency) || !payment.PaidAt.Equal(paidAt) {
			return errors.New("local payment mismatch")
		}
		local, e := h.store.GetFinanceInvoice(ctx, payment.InvoiceID)
		if e != nil || local.SubscriptionUUID != intent.SubscriptionID || local.AccountUUID != intent.AccountID || local.ProviderInvoiceID != invoice.ID {
			return errors.New("local invoice ownership mismatch")
		}
		var refunded int64
		for _, r := range refunds {
			if r.PaymentID == payment.ID {
				if r.Currency != payment.Currency || r.AmountMinor > payment.AmountMinor-refunded {
					return store.ErrFinanceRefundExceedsPayment
				}
				refunded += r.AmountMinor
			}
		}
		intent.Payments = append(intent.Payments, refundReviewPayment{payment.ID, invoice.ID, pi, payment.Currency, payment.AmountMinor, refunded, paidAt})
	}
	if first.IsZero() || len(intent.Payments) == 0 || firstSubscription != intent.ExternalID {
		return errors.New("first subscription history unavailable or ineligible")
	}
	intent.FirstPaidAt = &first
	return nil
}
