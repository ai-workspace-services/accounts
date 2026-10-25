package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxReferralReconcileInvoices = 50

// ReferralReconcileRequest is an explicit, bounded provider-invoice review.
// Apply defaults to false. This is a package operation, not an HTTP endpoint.
type ReferralReconcileRequest struct {
	AccountUUID string
	InvoiceIDs  []string
	Apply       bool
}

type ReferralReconcileItem struct {
	InvoiceID string    `json:"invoiceId"`
	Status    string    `json:"status"` // candidate, skipped, or reconciled; reconciled does not imply a new award
	PaidAt    time.Time `json:"paidAt"`
	PaidMinor int64     `json:"paidMinor"`
	Currency  string    `json:"currency"`
}

// reconcileReferralPaidInvoices previews candidates without writing local facts.
// Preview deliberately does not promise an award: payment-time inviter coverage,
// local risk tombstones, previous awards and concurrent changes are authoritatively
// checked by CreateReferralReward during apply. Apply fetches fresh provider facts
// and uses the same idempotent ledger path as invoice.paid; it never resets quota
// or changes the provider's billing schedule. Callers must authorize the account
// and explicitly retain the reviewed invoice IDs before setting Apply.
func (h *handler) reconcileReferralPaidInvoices(ctx context.Context, req ReferralReconcileRequest) ([]ReferralReconcileItem, error) {
	if h.stripe == nil || !h.stripe.enabled() {
		return nil, errors.New("Stripe configuration is unavailable")
	}
	account := strings.TrimSpace(req.AccountUUID)
	if account == "" || len(req.InvoiceIDs) == 0 || len(req.InvoiceIDs) > maxReferralReconcileInvoices {
		return nil, errors.New("account and between 1 and 50 invoice IDs are required")
	}
	ids := make([]string, 0, len(req.InvoiceIDs))
	seen := map[string]bool{}
	for _, raw := range req.InvoiceIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			return nil, errors.New("invoice IDs must be nonempty and unique")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	items := make([]ReferralReconcileItem, 0, len(ids))
	for _, id := range ids {
		invoice, err := h.stripe.fetchInvoice(ctx, id)
		if err != nil {
			return items, fmt.Errorf("invoice %s: %w", id, err)
		}
		if invoice.ID != id {
			return items, errors.New("Stripe returned a different invoice")
		}
		subscriptionID := invoiceSubscriptionID(invoice)
		if subscriptionID == "" {
			return items, errors.New("invoice is not a subscription payment")
		}
		sub, err := h.stripe.fetchSubscription(ctx, subscriptionID)
		if err != nil {
			return items, fmt.Errorf("invoice %s: %w", id, err)
		}
		if sub.ID != subscriptionID || strings.TrimSpace(sub.Metadata["user_id"]) != account || customerIDFromAny(invoice.Customer) != customerIDFromAny(sub.Customer) {
			return items, errors.New("invoice account or customer does not match the reviewed account")
		}
		item := ReferralReconcileItem{InvoiceID: id, Status: "skipped"}
		if !strings.EqualFold(invoice.Status, "paid") || invoice.AmountPaid <= 0 || invoice.PaidOutOfBand || customerIDFromAny(invoice.PaymentIntent) == "" || invoice.StatusTransitions.PaidAt <= 0 {
			items = append(items, item)
			continue
		}
		item.PaidAt = time.Unix(invoice.StatusTransitions.PaidAt, 0).UTC()
		planID, err := h.referralPlanFromInvoice(ctx, invoice)
		if err != nil {
			return items, err
		}
		campaign, err := h.referralCampaignAt(ctx, item.PaidAt)
		if err != nil {
			return items, err
		}
		if planID == "" || campaign == nil || !campaign.Active(item.PaidAt) {
			items = append(items, item)
			continue
		}
		if _, _, ok := referralInvoicePeriod(invoice); !ok {
			return items, errors.New("invoice has no consistent subscription service period")
		}
		item.Currency = strings.ToUpper(strings.TrimSpace(invoice.Currency))
		if !validReferralCurrency(item.Currency) {
			return items, errors.New("invoice currency is invalid")
		}
		item.PaidMinor, err = stripeAmountToCurrencyMinor(item.Currency, invoice.AmountPaid)
		if err != nil {
			return items, err
		}
		if _, err := h.store.GetReferralReferrer(ctx, account); err != nil {
			return items, err
		}
		first, err := h.stripe.isFirstPaidXConnectInvoice(ctx, h.store, customerIDFromAny(invoice.Customer), id, item.PaidAt)
		if err != nil {
			return items, err
		}
		if !first {
			items = append(items, item)
			continue
		}
		item.Status = "candidate"
		if req.Apply {
			if err := h.recordReferralPaidInvoice(ctx, invoice, sub, account); err != nil {
				return items, err
			}
			item.Status = "reconciled"
		}
		items = append(items, item)
	}
	return items, nil
}
