package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

// A quote is user-bound and short-lived. Its proration timestamp is reused
// both when checking the preview and updating Stripe's existing item.
type subscriptionQuote struct {
	UserID         string `json:"userId"`
	PlanID         string `json:"planId"`
	PriceID        string `json:"priceId"`
	Action         string `json:"action"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	ItemID         string `json:"itemId,omitempty"`
	FromPriceID    string `json:"fromPriceId,omitempty"`
	ProrationDate  int64  `json:"prorationDate"`
	ExpiresAt      int64  `json:"expiresAt"`
	AmountDue      int64  `json:"amountDue"`
	Currency       string `json:"currency"`
	RenewalAmount  int64  `json:"renewalAmount"`
	RenewalAt      int64  `json:"renewalAt"`
	PeriodStart    int64  `json:"periodStart,omitempty"`
	StateVersion   int64  `json:"stateVersion"`
}

// Retain the exact submitted inputs for an explicitly requested idempotent
// retry. In particular, account edits must not change customer_email on replay.
type subscriptionPurchaseRequest struct {
	subscriptionQuote
	SourcePath    string `json:"sourcePath"`
	CheckoutEmail string `json:"checkoutEmail"`
	QuoteProof    string `json:"quoteProof"`
}

func (c *stripeClient) signSubscriptionQuote(q subscriptionQuote) string {
	raw, _ := json.Marshal(q)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(c.secretKey))
	mac.Write([]byte("subscription-quote:v1:" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (c *stripeClient) readSubscriptionQuote(token, userID string) (subscriptionQuote, error) {
	var q subscriptionQuote
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(token) > 8192 {
		return q, errors.New("invalid quote")
	}
	mac := hmac.New(sha256.New, []byte(c.secretKey))
	mac.Write([]byte("subscription-quote:v1:" + parts[0]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		return q, errors.New("invalid quote")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return q, err
	}
	if err = json.Unmarshal(raw, &q); err != nil {
		return q, err
	}
	if q.UserID != userID || q.ExpiresAt <= time.Now().Unix() || q.ProrationDate > time.Now().Unix()+5 {
		return q, errors.New("quote expired or belongs to another account")
	}
	return q, nil
}
func hasPendingUpdate(sub *stripeSubscription) bool {
	return len(sub.PendingUpdate) > 0 && string(sub.PendingUpdate) != "null"
}

func (h *handler) currentPurchaseSubscription(ctx context.Context, userID string) (*store.Subscription, error) {
	subs, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var active *store.Subscription
	for i := range subs {
		s := &subs[i]
		if s.Kind != "subscription" {
			continue
		}
		if s.PlanID == store.BillingPlanFree {
			continue
		}
		switch s.Status {
		case "canceled", "cancelled", "incomplete_expired", "expired", "refunded":
			continue
		}
		if s.Status != "active" || s.Provider != "stripe" || !strings.HasPrefix(s.ExternalID, "sub_") || active != nil {
			return nil, errors.New("subscription requires reconciliation before another purchase")
		}
		active = s
	}
	return active, nil
}

func (h *handler) buildSubscriptionQuote(ctx context.Context, userID string, plan *store.BillingPlan, at int64) (subscriptionQuote, error) {
	q := subscriptionQuote{UserID: userID, PlanID: plan.PlanID, PriceID: plan.StripePriceID, Action: "subscribe", ProrationDate: at, ExpiresAt: at + 600}
	lifecycle, err := h.store.GetSubscriptionLifecycle(ctx, userID)
	if err != nil {
		return q, err
	}
	q.StateVersion = lifecycle.Version
	if !plan.Active || plan.Kind != "subscription" || plan.PriceAmount <= 0 || plan.StripePriceID == "" {
		return q, errors.New("plan unavailable")
	}
	current, err := h.currentPurchaseSubscription(ctx, userID)
	if err != nil {
		return q, err
	}
	form := url.Values{"subscription_details[items][0][price]": {plan.StripePriceID}, "subscription_details[items][0][quantity]": {"1"}}
	if current != nil {
		// Phase 1 only: an existing Plus monthly item becomes Pro monthly. Never
		// silently create a second subscription for downgrades or annual switches.
		if current.PlanID == plan.PlanID {
			return q, errors.New("already subscribed to this plan")
		}
		if current.PlanID != "PLUS" || plan.PlanID != "PRO-MONTHLY-V2" {
			return q, errors.New("this plan change is not supported yet")
		}
		old, err := h.store.GetBillingPlan(ctx, current.PlanID)
		if err != nil {
			return q, err
		}
		if old.PriceUnit != "month" || plan.PriceUnit != "month" || !strings.EqualFold(old.PriceCurrency, plan.PriceCurrency) || plan.PriceAmount <= old.PriceAmount || plan.IncludedQuotaBytes <= old.IncludedQuotaBytes {
			return q, errors.New("unsupported upgrade currency or period")
		}
		sub, err := h.stripe.fetchSubscription(ctx, current.ExternalID)
		if err != nil {
			return q, err
		}
		if sub.Status != "active" || sub.Metadata["user_id"] != userID || sub.CancelAtPeriodEnd || hasPendingUpdate(sub) || customerIDFromAny(sub.Schedule) != "" || sub.CollectionMethod != "charge_automatically" || len(sub.Items.Data) != 1 || sub.Items.Data[0].Quantity != 1 || sub.Items.Data[0].ID == "" || subscriptionPriceID(sub) != old.StripePriceID {
			return q, errors.New("subscription is not eligible for a monthly upgrade")
		}
		invoiceID := customerIDFromAny(sub.LatestInvoice)
		if invoiceID == "" {
			return q, errors.New("paid source invoice required")
		}
		invoice, err := h.stripe.fetchInvoice(ctx, invoiceID)
		if err != nil {
			return q, err
		}
		if invoice.Status != "paid" {
			return q, errors.New("settle the existing invoice before upgrading")
		}
		start, end := subscriptionPeriod(sub)
		if (sub.CurrentPeriodStart <= 0 && sub.Items.Data[0].CurrentPeriodStart <= 0) || end.Unix() <= at || start.Unix() > at {
			return q, errors.New("invalid active period")
		}
		profile, e := h.store.GetAccountBillingProfile(ctx, userID)
		if e != nil {
			return q, e
		}
		quota, e := h.store.GetAccountQuotaState(ctx, userID)
		if e != nil {
			return q, e
		}
		if profile == nil || profile.IncludedQuotaBytes != old.IncludedQuotaBytes || quota == nil || quota.PeriodStart == nil || quota.PeriodEnd == nil || !quota.PeriodStart.Equal(start) || !quota.PeriodEnd.Equal(end) {
			return q, errors.New("source allowance requires reconciliation")
		}
		q.Action = "upgrade"
		q.SubscriptionID = sub.ID
		q.ItemID = sub.Items.Data[0].ID
		q.FromPriceID = subscriptionPriceID(sub)
		q.PeriodStart = start.Unix()
		q.RenewalAt = end.Unix()
		form.Set("subscription", sub.ID)
		form.Set("subscription_details[items][0][id]", q.ItemID)
		form.Set("subscription_details[proration_behavior]", "always_invoice")
		form.Set("subscription_details[proration_date]", strconv.FormatInt(at, 10))
	}
	var invoice stripeInvoice
	if err = h.stripe.doForm(ctx, http.MethodPost, "/invoices/create_preview", form, &invoice); err != nil {
		return q, err
	}
	if !strings.EqualFold(invoice.Currency, plan.PriceCurrency) || invoice.AmountDue < 0 {
		return q, errors.New("unexpected invoice currency or total")
	}
	q.AmountDue = invoice.AmountDue
	q.Currency = strings.ToUpper(invoice.Currency)
	// Stripe computes the recurring total (discounts/tax included), not Portal.
	recurring := url.Values{}
	for k, v := range form {
		if !strings.Contains(k, "proration_") {
			recurring[k] = v
		}
	}
	recurring.Set("preview_mode", "recurring")
	var renewal stripeInvoice
	if err = h.stripe.doForm(ctx, http.MethodPost, "/invoices/create_preview", recurring, &renewal); err != nil {
		return q, err
	}
	if !strings.EqualFold(renewal.Currency, invoice.Currency) || renewal.Total < 0 {
		return q, errors.New("invalid recurring preview")
	}
	q.RenewalAmount = renewal.Total
	return q, nil
}

func (h *handler) stripeSubscriptionPreview(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if !user.MFAEnabled || h.isReadOnlyAccount(user) {
		respondError(c, 403, "mfa_required", "MFA is required before payment")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, 503, "stripe_not_configured", "Stripe unavailable")
		return
	}
	var req stripeCheckoutRequest
	if c.ShouldBindJSON(&req) != nil || req.Mode != "subscription" || req.ProductSlug != "xconnect" || !h.validateCheckoutPlan(c.Request.Context(), req) {
		respondError(c, 400, "invalid_billing_plan", "Invalid subscription plan")
		return
	}
	if err := h.validateBillingModeCheckout(c.Request.Context(), user.ID, req); err != nil {
		respondError(c, 409, "billing_mode_conflict", err.Error())
		return
	}
	if pending, err := h.reconcileSubscriptionPurchase(c.Request.Context(), user.ID); err != nil || pending != nil {
		respondError(c, 409, "purchase_pending", "Payment or subscription synchronization is in progress")
		return
	}
	plan, err := h.store.GetBillingPlan(c.Request.Context(), req.PlanID)
	if err != nil {
		respondError(c, 400, "invalid_billing_plan", "Plan unavailable")
		return
	}
	quote, err := h.buildSubscriptionQuote(c.Request.Context(), user.ID, plan, time.Now().Unix())
	if err != nil {
		respondError(c, 409, "subscription_change_unavailable", "This subscription change is unavailable; refresh the account or settle its current invoice")
		return
	}
	lifecycle, err := h.store.GetSubscriptionLifecycle(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, 503, "subscription_state_unavailable", "Cannot verify subscription state")
		return
	}
	if err := store.CheckSubscriptionAction(*lifecycle, quote.Action, time.Now().UTC()); err != nil {
		c.JSON(409, gin.H{"error": "subscription_change_blocked", "cooldownUntil": lifecycle.CooldownUntil(), "pendingPlanId": lifecycle.PendingPlanID})
		return
	}
	c.JSON(200, gin.H{"action": quote.Action, "planId": quote.PlanID, "amountDue": quote.AmountDue, "currency": quote.Currency, "renewalAmount": quote.RenewalAmount, "renewalAt": quote.RenewalAt, "effectiveAt": quote.ProrationDate, "expiresAt": quote.ExpiresAt, "previewToken": h.stripe.signSubscriptionQuote(quote), "stateVersion": lifecycle.Version, "cooldownUntil": lifecycle.CooldownUntil()})
}

func (h *handler) executeSubscriptionPurchase(c *gin.Context, user *store.User, req stripeCheckoutRequest) {
	ctx := c.Request.Context()
	q, err := h.stripe.readSubscriptionQuote(req.PreviewToken, user.ID)
	if err != nil || q.PlanID != req.PlanID || q.PriceID != req.StripePriceID || req.ProductSlug != "xconnect" {
		respondError(c, 409, "preview_required", "Preview and confirm the subscription before payment")
		return
	}
	if pending, err := h.reconcileSubscriptionPurchase(ctx, user.ID); err != nil || pending != nil {
		respondError(c, 409, "purchase_pending", "Payment or subscription synchronization is in progress")
		return
	}
	plan, err := h.store.GetBillingPlan(ctx, req.PlanID)
	if err != nil {
		respondError(c, 409, "plan_changed", "Refresh the plan preview")
		return
	}
	verified, err := h.buildSubscriptionQuote(ctx, user.ID, plan, q.ProrationDate)
	if err != nil || verified != q {
		respondError(c, 409, "quote_changed", "The quote changed; preview again before paying")
		return
	}
	raw, _ := json.Marshal(subscriptionPurchaseRequest{subscriptionQuote: q, SourcePath: req.SourcePath, CheckoutEmail: user.Email, QuoteProof: req.PreviewToken})
	op, err := h.store.ReserveSubscriptionPurchase(ctx, user.ID, raw)
	if err != nil {
		respondError(c, 409, "purchase_pending", "Another purchase is already pending")
		return
	}
	// Recheck local subscription state after obtaining the account reservation.
	// A webhook might have completed between preview and reservation.
	current, err := h.currentPurchaseSubscription(ctx, user.ID)
	if err != nil || q.Action == "subscribe" && current != nil || q.Action == "upgrade" && (current == nil || current.ExternalID != q.SubscriptionID || current.PlanID != "PLUS") {
		_ = h.store.UpdateSubscriptionPurchase(ctx, op.ID, "", json.RawMessage(`{"outcome":"not_submitted"}`), true)
		respondError(c, 409, "subscription_changed", "Refresh the subscription state")
		return
	}
	h.submitSubscriptionPurchase(c, user, op)
}

func retryableSubscriptionPurchase(op *store.FinanceOperation) bool {
	return op != nil && op.ProviderOperationID == "" && time.Since(op.CreatedAt) >= 0 && time.Since(op.CreatedAt) < 20*time.Minute
}

// Replay only this same account reservation and same provider idempotency key.
// Never release an ambiguous outcome or mint a second purchase to retry it.
func (h *handler) stripeSubscriptionRetry(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if !user.MFAEnabled || h.isReadOnlyAccount(user) {
		respondError(c, 403, "mfa_required", "MFA is required before payment")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, 503, "stripe_not_configured", "Stripe unavailable")
		return
	}
	op, err := h.store.PendingSubscriptionPurchase(c.Request.Context(), user.ID)
	if err != nil || !retryableSubscriptionPurchase(op) {
		respondError(c, 409, "purchase_retry_unavailable", "Resume the existing payment or request reconciliation")
		return
	}
	var stored subscriptionPurchaseRequest
	if json.Unmarshal(op.Request, &stored) != nil || stored.UserID != user.ID {
		respondError(c, 409, "purchase_retry_unavailable", "Purchase provenance needs reconciliation")
		return
	}
	current, err := h.currentPurchaseSubscription(c.Request.Context(), user.ID)
	if err != nil || stored.Action == "subscribe" && current != nil || stored.Action == "upgrade" && (current == nil || current.ExternalID != stored.SubscriptionID || current.PlanID != "PLUS") {
		respondError(c, 409, "purchase_retry_unavailable", "Subscription state changed; reconcile the original payment")
		return
	}
	if err := h.validateBillingModeCheckout(c.Request.Context(), user.ID, stripeCheckoutRequest{Mode: "subscription"}); err != nil {
		respondError(c, 409, "billing_mode_conflict", "Reconcile the current billing mode before retrying")
		return
	}
	h.submitSubscriptionPurchase(c, user, op)
}

func (h *handler) submitSubscriptionPurchase(c *gin.Context, user *store.User, op *store.FinanceOperation) {
	ctx := c.Request.Context()
	var stored subscriptionPurchaseRequest
	if json.Unmarshal(op.Request, &stored) != nil || stored.UserID != user.ID || stored.CheckoutEmail == "" {
		respondError(c, 409, "purchase_retry_unavailable", "Purchase provenance needs reconciliation")
		return
	}
	q := stored.subscriptionQuote
	// A credential/account change must not replay this operation against a
	// different Stripe authority. Quote expiry was checked before reservation;
	// this bounded retry checks its original proof, not a newly issued quote.
	if !hmac.Equal([]byte(stored.QuoteProof), []byte(h.stripe.signSubscriptionQuote(q))) {
		respondError(c, 409, "purchase_retry_unavailable", "Payment authority changed; reconcile the original request")
		return
	}
	req := stripeCheckoutRequest{PlanID: q.PlanID, StripePriceID: q.PriceID, Mode: "subscription", ProductSlug: "xconnect", SourcePath: stored.SourcePath, OperationKey: op.IdempotencyKey, SessionExpiresAt: op.CreatedAt.Add(time.Hour).Unix()}
	checkoutUser := *user
	checkoutUser.Email = stored.CheckoutEmail
	var err error
	result := stripeSessionResponse{}
	providerID := ""
	if q.Action == "subscribe" {
		session, e := h.stripe.createCheckoutSession(ctx, &checkoutUser, req)
		err = e
		if e == nil {
			result = *session
			providerID = session.ID
		}
	} else {
		form := url.Values{"items[0][id]": {q.ItemID}, "items[0][price]": {q.PriceID}, "proration_behavior": {"always_invoice"}, "proration_date": {strconv.FormatInt(q.ProrationDate, 10)}, "payment_behavior": {"pending_if_incomplete"}, "expand[]": {"latest_invoice"}}
		var sub stripeSubscription
		err = h.stripe.doForm(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(q.SubscriptionID), form, &sub, op.IdempotencyKey)
		if err == nil {
			if sub.ID != q.SubscriptionID {
				err = errors.New("provider subscription mismatch")
			} else {
				result.ID = sub.ID
				invoiceID := customerIDFromAny(sub.LatestInvoice)
				if invoiceID != "" {
					providerID = invoiceID
					var invoice *stripeInvoice
					invoice, err = h.stripe.fetchInvoice(ctx, invoiceID)
					if err == nil {
						result.URL = invoice.HostedInvoiceURL
					}
				}
			}
		}
	}
	if err != nil {
		// The provider may have accepted the request before a network failure.
		// Keep the durable reservation; never create another charge speculatively.
		_ = h.store.UpdateSubscriptionPurchase(context.WithoutCancel(ctx), op.ID, providerID, nil, false)
		respondError(c, 502, "payment_reconciliation_required", "Payment result needs reconciliation; do not purchase again")
		return
	}
	receipt, _ := json.Marshal(result)
	if err = h.store.UpdateSubscriptionPurchase(context.WithoutCancel(ctx), op.ID, providerID, receipt, false); err != nil {
		respondError(c, 503, "purchase_sync_pending", "Payment is being synchronized")
		return
	}
	c.JSON(200, gin.H{"id": result.ID, "url": result.URL, "pending": true})
}

// Status polling may release a reservation only after provider expiry, or
// after a paid provider result AND the matching local webhook projection.
func (h *handler) reconcileSubscriptionPurchase(ctx context.Context, userID string) (*store.FinanceOperation, error) {
	op, err := h.store.PendingSubscriptionPurchase(ctx, userID)
	if err != nil || op == nil {
		return op, err
	}
	if op.ProviderOperationID == "" || h.stripe == nil {
		return op, nil
	}
	var q subscriptionQuote
	if json.Unmarshal(op.Request, &q) != nil || q.UserID != userID {
		return op, errors.New("purchase provenance mismatch")
	}
	providerSub := ""
	var settled *stripeSubscription
	expired := false
	paid := false
	if strings.HasPrefix(op.ProviderOperationID, "cs_") {
		var session stripeCheckoutSession
		if err = h.stripe.doJSON(ctx, http.MethodGet, "/checkout/sessions/"+url.PathEscape(op.ProviderOperationID), &session); err != nil {
			return op, err
		}
		if session.ID != op.ProviderOperationID || session.Metadata["user_id"] != userID || session.Metadata["plan_id"] != q.PlanID {
			return op, errors.New("checkout identity mismatch")
		}
		expired = session.Status == "expired"
		paid = session.Status == "complete" && session.PaymentStatus == "paid"
		providerSub = session.Subscription
		if session.URL != "" {
			op.Response, _ = json.Marshal(stripeSessionResponse{ID: session.ID, URL: session.URL})
		}
		if paid {
			settled, err = h.stripe.fetchSubscription(ctx, providerSub)
			if err != nil {
				return op, err
			}
			paid = settled.ID == providerSub && settled.Metadata["user_id"] == userID && settled.Status == "active" && subscriptionPriceID(settled) == q.PriceID && !hasPendingUpdate(settled)
		}
	} else if strings.HasPrefix(op.ProviderOperationID, "in_") {
		sub, e := h.stripe.fetchSubscription(ctx, q.SubscriptionID)
		if e != nil {
			return op, e
		}
		if sub.ID != q.SubscriptionID || sub.Metadata["user_id"] != userID {
			return op, errors.New("subscription identity mismatch")
		}
		providerSub = sub.ID
		settled = sub
		invoiceID := op.ProviderOperationID
		if invoiceID != "" {
			invoice, e := h.stripe.fetchInvoice(ctx, invoiceID)
			if e != nil {
				return op, e
			}
			if invoice.ID != invoiceID || invoiceSubscriptionID(invoice) != q.SubscriptionID {
				return op, errors.New("invoice identity mismatch")
			}
			if invoice.HostedInvoiceURL != "" {
				op.Response, _ = json.Marshal(stripeSessionResponse{ID: invoiceID, URL: invoice.HostedInvoiceURL})
			}
			paid = invoice.Status == "paid" && subscriptionPriceID(sub) == q.PriceID && !hasPendingUpdate(sub)
			expired = !hasPendingUpdate(sub) && subscriptionPriceID(sub) == q.FromPriceID && invoice.Status == "void"
		}
	}
	if paid {
		subs, e := h.store.ListSubscriptionsByUser(ctx, userID)
		if e != nil {
			return op, e
		}
		projected := false
		for _, sub := range subs {
			if sub.ExternalID == providerSub && sub.PlanID == q.PlanID && sub.Status == "active" {
				projected = true
			}
		}
		profile, e := h.store.GetAccountBillingProfile(ctx, userID)
		if e != nil {
			if errors.Is(e, store.ErrUserNotFound) {
				return op, nil
			}
			return op, e
		}
		plan, e := h.store.GetBillingPlan(ctx, q.PlanID)
		if e != nil {
			return op, e
		}
		quota, e := h.store.GetAccountQuotaState(ctx, userID)
		if e != nil {
			if errors.Is(e, store.ErrUserNotFound) {
				return op, nil
			}
			return op, e
		}
		start, end := subscriptionQuotaPeriod(plan, settled, time.Now())
		paid = projected && profile != nil && profile.IncludedQuotaBytes == plan.IncludedQuotaBytes && profile.PricingRuleVersion == "plan:"+q.PlanID && quota != nil && quota.PeriodStart != nil && quota.PeriodEnd != nil && quota.PeriodStart.Equal(start) && quota.PeriodEnd.Equal(end)
	}
	if expired || paid {
		outcome := "webhook_confirmed"
		if expired {
			outcome = "expired"
		}
		receipt, _ := json.Marshal(map[string]string{"outcome": outcome})
		if err = h.store.UpdateSubscriptionPurchase(ctx, op.ID, op.ProviderOperationID, receipt, true); err != nil {
			return op, err
		}
		return nil, nil
	}
	return op, nil
}
func (h *handler) stripeSubscriptionPending(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	op, err := h.reconcileSubscriptionPurchase(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, 503, "purchase_status_unavailable", "Cannot verify payment status")
		return
	}
	if op == nil {
		lifecycle, e := h.store.GetSubscriptionLifecycle(c.Request.Context(), user.ID)
		if e != nil {
			respondError(c, 503, "subscription_state_unavailable", "Cannot verify subscription state")
			return
		}
		current, e := h.currentPurchaseSubscription(c.Request.Context(), user.ID)
		if e != nil {
			c.JSON(200, gin.H{"pending": true})
			return
		}
		planID := ""
		if current != nil {
			planID = current.PlanID
		}
		c.JSON(200, gin.H{"pending": lifecycle.OperationID != "", "planId": planID, "stateVersion": lifecycle.Version, "cooldownUntil": lifecycle.CooldownUntil(), "pendingPlanId": lifecycle.PendingPlanID, "effectiveAt": lifecycle.EffectiveAt, "refundReviewPending": lifecycle.RefundReviewPending})
		return
	}
	var q subscriptionQuote
	_ = json.Unmarshal(op.Request, &q)
	var receipt stripeSessionResponse
	_ = json.Unmarshal(op.Response, &receipt)
	c.JSON(200, gin.H{"pending": true, "planId": q.PlanID, "url": receipt.URL, "retryable": retryableSubscriptionPurchase(op)})
}
