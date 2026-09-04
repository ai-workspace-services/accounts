package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"account/internal/store"
)

// Entitlement sync (billing P1): translates subscription lifecycle events into
// the account_billing_profiles / account_quota_states rows billing-service
// rates against. Decisions (2026-07-11): sync lives inline in accounts and is
// driven by Stripe webhooks; billing-service never talks to Stripe.

// resolveBillingPlan looks a plan up by Stripe price id first, then by the
// plan_id carried in Stripe metadata.
func (h *handler) resolveBillingPlan(ctx context.Context, priceID, planID string) (*store.BillingPlan, error) {
	if strings.TrimSpace(priceID) != "" {
		plan, err := h.store.GetBillingPlanByPriceID(ctx, priceID)
		if err == nil {
			return plan, nil
		}
		if !errors.Is(err, store.ErrBillingPlanNotFound) {
			return nil, err
		}
	}
	if strings.TrimSpace(planID) != "" {
		plan, err := h.store.GetBillingPlan(ctx, planID)
		if err == nil {
			return plan, nil
		}
		if !errors.Is(err, store.ErrBillingPlanNotFound) {
			return nil, err
		}
	}
	return nil, store.ErrBillingPlanNotFound
}

// applyPlanEntitlements writes the billing profile for a user from the plan
// catalog. The profile is what billing-service prices minute buckets against.
func (h *handler) applyPlanEntitlements(ctx context.Context, userID string, plan *store.BillingPlan) error {
	if plan == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	packageName := strings.TrimSpace(plan.PackageName)
	if packageName == "" {
		packageName = "default"
	}
	profile := &store.AccountBillingProfile{
		AccountUUID:        userID,
		PackageName:        packageName,
		IncludedQuotaBytes: plan.IncludedQuotaBytes,
		RegionMultiplier:   plan.Multiplier("region"),
		LineMultiplier:     plan.Multiplier("line"),
		PeakMultiplier:     plan.Multiplier("peak"),
		OffPeakMultiplier:  plan.Multiplier("offpeak"),
		PricingRuleVersion: fmt.Sprintf("plan:%s", plan.PlanID),
	}
	// Preserve an operator-tuned base price when one exists; the catalog does
	// not own per-byte pricing yet (billing-service defaults apply otherwise).
	if existing, err := h.store.GetAccountBillingProfile(ctx, userID); err == nil && existing != nil {
		profile.BasePricePerByte = existing.BasePricePerByte
	}
	return h.store.UpsertAccountBillingProfile(ctx, profile)
}

// resetQuotaForPlan re-arms the quota state for a fresh billing period
// (subscription activation or invoice.paid renewal) and clears dunning flags.
// periodBounds bound the grant so usage/summary can report "used this period"
// and a reset date. Production callers pass exactly two values. The optional
// form keeps older internal tests/callers source-compatible and uses the
// natural-month fallback until they migrate to explicit bounds.
func (h *handler) resetQuotaForPlan(ctx context.Context, userID string, plan *store.BillingPlan, periodBounds ...time.Time) error {
	if plan == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	now := time.Now().UTC()
	state := &store.AccountQuotaState{AccountUUID: userID}
	if existing, err := h.store.GetAccountQuotaState(ctx, userID); err == nil && existing != nil {
		state = existing
	}
	state.RemainingIncludedQuota = plan.IncludedQuotaBytes
	state.Arrears = false
	state.ArrearsSince = nil
	state.ThrottleState = "normal"
	state.SuspendState = "active"
	periodStart, periodEnd := naturalMonthPeriod(now)
	if len(periodBounds) == 2 && periodBounds[1].After(periodBounds[0]) {
		periodStart = periodBounds[0].UTC()
		periodEnd = periodBounds[1].UTC()
	}
	start := periodStart.UTC()
	end := periodEnd.UTC()
	state.PeriodStart = &start
	state.PeriodEnd = &end
	state.EffectiveAt = now
	return h.store.UpsertAccountQuotaState(ctx, state)
}

// naturalMonthPeriod is the period fallback for grants with no Stripe
// subscription to source current_period_start/end from (e.g. the FREE plan
// after a subscription ends).
func naturalMonthPeriod(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end
}

// markAccountArrears flags a payment failure. Escalation to throttled and
// suspended is time-based and owned by billing-service (P1.5), which reads
// ArrearsSince to measure how long the current episode has run; repeated
// failures within the same episode must not push that clock forward.
func (h *handler) markAccountArrears(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return nil
	}
	now := time.Now().UTC()
	state := &store.AccountQuotaState{
		AccountUUID:   userID,
		ThrottleState: "normal",
		SuspendState:  "active",
		ArrearsSince:  &now,
		EffectiveAt:   now,
	}
	if existing, err := h.store.GetAccountQuotaState(ctx, userID); err == nil && existing != nil {
		state = existing
		if state.ArrearsSince == nil {
			state.ArrearsSince = &now
		}
	}
	state.Arrears = true
	return h.store.UpsertAccountQuotaState(ctx, state)
}

// downgradeToFreePlan applies the FREE catalog entry (or a zeroed default
// profile when the catalog has none) after a subscription ends.
func (h *handler) downgradeToFreePlan(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return nil
	}
	plan, err := h.store.GetBillingPlan(ctx, store.BillingPlanFree)
	if err != nil {
		if !errors.Is(err, store.ErrBillingPlanNotFound) {
			return err
		}
		plan = &store.BillingPlan{PlanID: store.BillingPlanFree, PackageName: "default"}
	}
	if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
		return err
	}
	periodStart, periodEnd := naturalMonthPeriod(time.Now())
	return h.resetQuotaForPlan(ctx, userID, plan, periodStart, periodEnd)
}

// supersedeActiveTrials marks the user's active trial subscriptions as
// superseded once a paid subscription takes over.
func (h *handler) supersedeActiveTrials(ctx context.Context, userID string) {
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		slog.Warn("failed to list subscriptions while superseding trial", "err", err, "userID", userID)
		return
	}
	for i := range subscriptions {
		sub := subscriptions[i]
		if !strings.EqualFold(strings.TrimSpace(sub.Kind), "trial") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(sub.Status), "active") {
			continue
		}
		sub.Status = "superseded"
		if err := h.store.UpsertSubscription(ctx, &sub); err != nil {
			slog.Warn("failed to supersede trial subscription", "err", err, "userID", userID, "externalID", sub.ExternalID)
		}
	}
}

// publishBillingEvent enqueues a lifecycle notification on the PGMQ
// billing_events queue. Best-effort: consumers (billing-service reconcile,
// dunning, notifications) must tolerate gaps and the webhook flow never
// fails because the queue is unavailable.
func (h *handler) publishBillingEvent(ctx context.Context, event *store.BillingEvent) {
	if event == nil {
		return
	}
	if err := h.store.PublishBillingEvent(ctx, event); err != nil {
		slog.Warn("failed to publish billing event", "err", err, "type", event.Type, "userID", event.UserID)
	}
}
