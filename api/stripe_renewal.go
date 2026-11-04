package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Schedules are authoritative for future changes. Local finance operations only
// serialize requests and retain the provider receipt across ambiguous failures.
type renewalPhase struct {
	Start int64 `json:"start_date"`
	End   int64 `json:"end_date"`
	Items []struct {
		Price    string `json:"price"`
		Quantity int64  `json:"quantity"`
	} `json:"items"`
}
type renewalSchedule struct {
	ID                   string            `json:"id"`
	Status               string            `json:"status"`
	EndBehavior          string            `json:"end_behavior"`
	Subscription         string            `json:"subscription"`
	ReleasedSubscription string            `json:"released_subscription"`
	Metadata             map[string]string `json:"metadata"`
	Phases               []renewalPhase    `json:"phases"`
}

func (h *handler) renewalQuote(ctx context.Context, userID, planID string, at int64) (subscriptionQuote, error) {
	q := subscriptionQuote{UserID: userID, PlanID: planID, Action: "renewal", ProrationDate: at, ExpiresAt: at + 600}
	plan, err := h.store.GetBillingPlan(ctx, planID)
	if err != nil || plan == nil || !plan.Active || plan.Kind != "subscription" || !strings.HasPrefix(plan.StripePriceID, "price_") {
		return q, errors.New("target plan unavailable")
	}
	current, err := h.currentPurchaseSubscription(ctx, userID)
	if err != nil || current == nil {
		return q, errors.New("one active Stripe subscription is required")
	}
	sub, err := h.stripe.fetchSubscription(ctx, current.ExternalID)
	if err != nil {
		return q, err
	}
	if sub.ID != current.ExternalID || sub.Metadata["user_id"] != userID || sub.Status != "active" || sub.CancelAtPeriodEnd || hasPendingUpdate(sub) || customerIDFromAny(sub.Schedule) != "" || sub.CollectionMethod != "charge_automatically" || len(sub.Items.Data) != 1 || sub.Items.Data[0].Quantity != 1 || !strings.HasPrefix(subscriptionPriceID(sub), "price_") {
		return q, errors.New("subscription is not eligible for renewal scheduling")
	}
	if len(sub.Discounts) > 0 || len(sub.DefaultTaxRates) > 0 || sub.AutomaticTax.Enabled || len(sub.Items.Data[0].Discounts) > 0 || len(sub.Items.Data[0].TaxRates) > 0 || sub.ApplicationFeePercent != nil || sub.TrialEnd != nil || (len(sub.TransferData) > 0 && string(sub.TransferData) != "null") {
		return q, errors.New("complex billing settings require a reviewed migration")
	}
	start, end := subscriptionPeriod(sub)
	if (sub.CurrentPeriodStart <= 0 && sub.Items.Data[0].CurrentPeriodStart <= 0) || !end.After(time.Unix(at, 0)) || start.After(time.Unix(at, 0)) {
		return q, errors.New("verified subscription period required")
	}
	invoice, err := h.stripe.fetchInvoice(ctx, customerIDFromAny(sub.LatestInvoice))
	if err != nil || invoice.Status != "paid" || invoiceSubscriptionID(invoice) != sub.ID || !strings.EqualFold(invoice.Currency, plan.PriceCurrency) {
		return q, errors.New("current subscription invoice must be paid")
	}
	var price struct {
		ID            string `json:"id"`
		Active        bool   `json:"active"`
		Currency      string `json:"currency"`
		UnitAmount    int64  `json:"unit_amount"`
		BillingScheme string `json:"billing_scheme"`
		Recurring     struct {
			Interval string `json:"interval"`
			Count    int    `json:"interval_count"`
			Usage    string `json:"usage_type"`
		} `json:"recurring"`
	}
	if err = h.stripe.doJSON(ctx, http.MethodGet, "/prices/"+url.PathEscape(plan.StripePriceID), &price); err != nil {
		return q, err
	}
	if price.ID != plan.StripePriceID || !price.Active || price.UnitAmount != plan.PriceAmount || !strings.EqualFold(price.Currency, plan.PriceCurrency) || price.Recurring.Interval != plan.PriceUnit || price.Recurring.Count != 1 || price.Recurring.Usage != "licensed" || price.BillingScheme != "per_unit" || (plan.PriceUnit != "month" && plan.PriceUnit != "year") || plan.PriceAmount <= 0 {
		return q, errors.New("Stripe price and catalog do not match")
	}
	if subscriptionPriceID(sub) == price.ID {
		return q, errors.New("already using this price")
	}
	lifecycle, err := h.store.GetSubscriptionLifecycle(ctx, userID)
	if err != nil {
		return q, err
	}
	if err = store.CheckSubscriptionAction(*lifecycle, "renewal", time.Now().UTC()); err != nil {
		return q, err
	}
	q.StateVersion = lifecycle.Version
	q.PriceID = price.ID
	q.FromPriceID = subscriptionPriceID(sub)
	q.SubscriptionID = sub.ID
	q.ItemID = sub.Items.Data[0].ID
	q.PeriodStart = start.Unix()
	q.RenewalAt = end.Unix()
	q.RenewalAmount = price.UnitAmount
	q.RenewalInterval = price.Recurring.Interval
	q.Currency = strings.ToUpper(price.Currency)
	return q, nil
}

func (h *handler) stripeRenewalPreview(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, 403, "read_only_account", "demo account is read-only")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, 503, "stripe_not_configured", "Stripe unavailable")
		return
	}
	var req struct {
		PlanID string `json:"planId"`
	}
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "planId required")
		return
	}
	q, err := h.renewalQuote(c.Request.Context(), user.ID, strings.TrimSpace(req.PlanID), time.Now().Unix())
	if err != nil {
		respondError(c, 409, "renewal_unavailable", "Cannot schedule this subscription or verify the catalog price")
		return
	}
	c.JSON(200, gin.H{"planId": q.PlanID, "amountDue": 0, "renewalAmount": q.RenewalAmount, "currency": q.Currency, "effectiveAt": q.RenewalAt, "previewToken": h.stripe.signSubscriptionQuote(q)})
}

func (h *handler) stripeRenewalApply(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, 403, "read_only_account", "demo account is read-only")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, 503, "stripe_not_configured", "Stripe unavailable")
		return
	}
	var req struct {
		Token string `json:"previewToken"`
	}
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "previewToken required")
		return
	}
	q, err := h.stripe.readSubscriptionQuote(req.Token, user.ID)
	if err != nil || q.Action != "renewal" {
		respondError(c, 409, "preview_required", "Preview the renewal change again")
		return
	}
	raw, _ := json.Marshal(q)
	digest := sha256.Sum256(raw)
	key := "subscription-renewal:" + hex.EncodeToString(digest[:])
	// A successful replay is checked before source state, because our own schedule
	// changes that state. The account gate serializes with purchase and cancellation.
	lifecycle, err := h.store.GetSubscriptionLifecycle(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, 503, "renewal_unavailable", "Cannot read subscription state")
		return
	}
	if lifecycle.Version == q.StateVersion {
		verified, e := h.renewalQuote(c.Request.Context(), user.ID, q.PlanID, q.ProrationDate)
		if e != nil || verified != q {
			respondError(c, 409, "quote_changed", "Preview the renewal change again")
			return
		}
	}
	op, err := h.store.ReserveSubscriptionAction(c.Request.Context(), user.ID, key, "renewal", raw)
	if err != nil {
		respondError(c, 409, "renewal_conflict", "A subscription action changed or is pending")
		return
	}
	if op.Status == store.FinanceOperationSucceeded {
		c.Data(200, "application/json", op.Response)
		return
	}
	if time.Since(op.CreatedAt) > 20*time.Minute {
		respondError(c, 409, "renewal_reconciliation_required", "The original provider operation needs reconciliation")
		return
	}
	schedule, err := h.stripe.scheduleRenewal(c.Request.Context(), q, key)
	if err != nil {
		respondError(c, 502, "renewal_reconciliation_required", "The schedule outcome needs reconciliation; retry the same confirmation")
		return
	}
	receipt, _ := json.Marshal(gin.H{"scheduleId": schedule.ID, "planId": q.PlanID, "effectiveAt": q.RenewalAt, "status": "scheduled"})
	if err = h.store.FinishSubscriptionAction(context.WithoutCancel(c.Request.Context()), user.ID, op.ID, schedule.ID, receipt, true, "", nil); err != nil {
		respondError(c, 503, "renewal_reconciliation_required", "Schedule confirmed; local receipt needs reconciliation")
		return
	}
	c.Data(200, "application/json", receipt)
}

func (s *stripeClient) scheduleRenewal(ctx context.Context, q subscriptionQuote, key string) (*renewalSchedule, error) {
	if q.RenewalInterval != "month" && q.RenewalInterval != "year" {
		return nil, errors.New("verified renewal interval required")
	}
	var created renewalSchedule
	if err := s.doForm(ctx, http.MethodPost, "/subscription_schedules", url.Values{"from_subscription": {q.SubscriptionID}}, &created, key+":create"); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(created.ID, "sub_sched_") || created.Subscription != q.SubscriptionID || len(created.Phases) != 1 || len(created.Phases[0].Items) != 1 || created.Phases[0].Items[0].Price != q.FromPriceID || created.Phases[0].Items[0].Quantity != 1 || created.Phases[0].Start != q.PeriodStart || created.Phases[0].End != q.RenewalAt {
		return nil, errors.New("source schedule mismatch")
	}
	form := url.Values{
		"end_behavior":                        {"release"},
		"proration_behavior":                  {"none"},
		"metadata[portal_renewal_user]":       {q.UserID},
		"metadata[portal_renewal_key]":        {key},
		"metadata[portal_renewal_plan]":       {q.PlanID},
		"phases[0][start_date]":               {strconv.FormatInt(q.PeriodStart, 10)},
		"phases[0][end_date]":                 {strconv.FormatInt(q.RenewalAt, 10)},
		"phases[0][items][0][price]":          {q.FromPriceID},
		"phases[0][items][0][quantity]":       {"1"},
		"phases[0][proration_behavior]":       {"none"},
		"phases[1][start_date]":               {strconv.FormatInt(q.RenewalAt, 10)},
		"phases[1][duration][interval]":       {q.RenewalInterval},
		"phases[1][duration][interval_count]": {"1"},
		"phases[1][items][0][price]":          {q.PriceID},
		"phases[1][items][0][quantity]":       {"1"},
		"phases[1][proration_behavior]":       {"none"},
		"phases[1][metadata][plan_id]":        {q.PlanID},
	}
	var updated renewalSchedule
	if err := s.doForm(ctx, http.MethodPost, "/subscription_schedules/"+url.PathEscape(created.ID), form, &updated, key+":update"); err != nil {
		return nil, err
	}
	if updated.ID != created.ID || updated.Subscription != q.SubscriptionID || updated.Status != "active" || updated.EndBehavior != "release" || updated.Metadata["portal_renewal_user"] != q.UserID || updated.Metadata["portal_renewal_key"] != key || updated.Metadata["portal_renewal_plan"] != q.PlanID || len(updated.Phases) != 2 || len(updated.Phases[0].Items) != 1 || len(updated.Phases[1].Items) != 1 || updated.Phases[0].Start != q.PeriodStart || updated.Phases[0].Items[0].Price != q.FromPriceID || updated.Phases[0].Items[0].Quantity != 1 || updated.Phases[0].End != q.RenewalAt || updated.Phases[1].Start != q.RenewalAt || updated.Phases[1].End <= q.RenewalAt || updated.Phases[1].Items[0].Price != q.PriceID || updated.Phases[1].Items[0].Quantity != 1 {
		return nil, errors.New("provider did not confirm requested phases")
	}
	return &updated, nil
}

func (h *handler) stripeRenewalStatus(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, 503, "stripe_not_configured", "Stripe unavailable")
		return
	}
	current, err := h.currentPurchaseSubscription(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, 409, "renewal_unavailable", "Subscription requires reconciliation")
		return
	}
	if current == nil {
		c.JSON(200, gin.H{"status": "none"})
		return
	}
	remote, err := h.stripe.fetchSubscription(c.Request.Context(), current.ExternalID)
	if err != nil || remote.Metadata["user_id"] != user.ID {
		respondError(c, 503, "renewal_unavailable", "Cannot verify subscription")
		return
	}
	id := customerIDFromAny(remote.Schedule)
	if id == "" {
		c.JSON(200, gin.H{"status": "none"})
		return
	}
	var schedule renewalSchedule
	if err = h.stripe.doJSON(c.Request.Context(), http.MethodGet, "/subscription_schedules/"+url.PathEscape(id), &schedule); err != nil || schedule.Subscription != remote.ID || schedule.Metadata["portal_renewal_user"] != user.ID || len(schedule.Phases) != 2 {
		respondError(c, 409, "renewal_unavailable", "Schedule needs reconciliation")
		return
	}
	if schedule.Phases[1].Start <= time.Now().Unix() {
		c.JSON(200, gin.H{"status": "applied", "planId": schedule.Metadata["portal_renewal_plan"]})
		return
	}
	c.JSON(200, gin.H{"status": "scheduled", "planId": schedule.Metadata["portal_renewal_plan"], "effectiveAt": schedule.Phases[1].Start})
}
