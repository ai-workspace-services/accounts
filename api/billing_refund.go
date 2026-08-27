package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const refundEligibilityWindow = 7 * 24 * time.Hour

type subscriptionRefundRequest struct {
	ExternalID string `json:"externalId"`
}

// refundSubscription applies the published first-subscription policy: a
// Stripe subscription bought within seven days is refundable only when its
// measured usage is below 5% of that plan's included allowance. Usage is read
// from the same minute buckets used for billing, never from a client value.
func (h *handler) refundSubscription(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}
	if !user.MFAEnabled {
		respondError(c, http.StatusForbidden, "mfa_required", "multi-factor authentication is required before requesting a refund")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, http.StatusServiceUnavailable, "stripe_not_configured", "stripe is not configured")
		return
	}
	var req subscriptionRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	var local *store.Subscription
	subs, err := h.store.ListSubscriptionsByUser(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "subscriptions_unavailable", "failed to load subscriptions")
		return
	}
	for i := range subs {
		if strings.TrimSpace(subs[i].ExternalID) == strings.TrimSpace(req.ExternalID) {
			local = &subs[i]
			break
		}
	}
	if local == nil || !strings.EqualFold(local.Provider, "stripe") || !strings.EqualFold(local.Kind, "subscription") {
		respondError(c, http.StatusNotFound, "subscription_not_found", "subscription not found")
		return
	}
	for i := range subs {
		prior := subs[i]
		if strings.EqualFold(prior.Provider, "stripe") && strings.EqualFold(prior.Kind, "subscription") && prior.ExternalID != local.ExternalID && !prior.CreatedAt.After(local.CreatedAt) {
			respondError(c, http.StatusUnprocessableEntity, "refund_not_first_subscription", "only a first subscription is eligible for an automated refund")
			return
		}
	}
	now := time.Now().UTC()
	if local.CreatedAt.IsZero() || now.Sub(local.CreatedAt) > refundEligibilityWindow {
		respondError(c, http.StatusUnprocessableEntity, "refund_window_expired", "this subscription is outside the seven-day refund window")
		return
	}
	plan, err := h.store.GetBillingPlan(c.Request.Context(), local.PlanID)
	if err != nil || plan.IncludedQuotaBytes <= 0 {
		respondError(c, http.StatusUnprocessableEntity, "refund_not_eligible", "this subscription is not eligible for an automated refund")
		return
	}
	buckets, err := h.store.ListTrafficMinuteBucketsByAccount(c.Request.Context(), user.ID, local.CreatedAt, now)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "usage_unavailable", "failed to verify usage for refund")
		return
	}
	var used int64
	for _, bucket := range buckets {
		used += bucket.TotalBytes
	}
	if float64(used) >= float64(plan.IncludedQuotaBytes)*0.05 {
		respondError(c, http.StatusUnprocessableEntity, "refund_usage_limit_exceeded", "usage exceeds the refund eligibility limit")
		return
	}
	remote, err := h.stripe.fetchSubscription(c.Request.Context(), local.ExternalID)
	if err != nil {
		respondError(c, http.StatusBadGateway, "stripe_subscription_lookup_failed", "failed to verify stripe subscription")
		return
	}
	invoiceID := customerIDFromAny(remote.LatestInvoice)
	if invoiceID == "" {
		respondError(c, http.StatusUnprocessableEntity, "refund_payment_not_found", "no refundable payment was found")
		return
	}
	invoice, err := h.stripe.fetchInvoice(c.Request.Context(), invoiceID)
	if err != nil {
		respondError(c, http.StatusBadGateway, "stripe_invoice_lookup_failed", "failed to verify stripe payment")
		return
	}
	paymentIntentID := customerIDFromAny(invoice.PaymentIntent)
	if paymentIntentID == "" {
		respondError(c, http.StatusUnprocessableEntity, "refund_payment_not_found", "no refundable payment was found")
		return
	}
	if err := h.stripe.refundPaymentIntent(c.Request.Context(), paymentIntentID, "subscription-refund:"+local.ExternalID); err != nil {
		respondError(c, http.StatusBadGateway, "stripe_refund_failed", "failed to issue stripe refund")
		return
	}
	if err := h.stripe.cancelSubscription(c.Request.Context(), local.ExternalID); err != nil {
		respondError(c, http.StatusBadGateway, "stripe_cancel_failed", "refund issued but subscription cancellation failed")
		return
	}
	if _, err := h.store.CancelSubscription(c.Request.Context(), user.ID, local.ExternalID, now); err != nil && !errors.Is(err, store.ErrSubscriptionNotFound) {
		respondError(c, http.StatusInternalServerError, "subscription_cancel_failed", "refund issued but local subscription update failed")
		return
	}
	if err := h.downgradeToFreePlan(c.Request.Context(), user.ID); err != nil {
		respondError(c, http.StatusInternalServerError, "refund_entitlement_update_failed", "refund issued but entitlement update failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"refunded": true})
}
