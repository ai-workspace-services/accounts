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
func (h *handler) resetQuotaForPlan(ctx context.Context, userID string, plan *store.BillingPlan) error {
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
	state.ThrottleState = "normal"
	state.SuspendState = "active"
	state.EffectiveAt = now
	return h.store.UpsertAccountQuotaState(ctx, state)
}

// markAccountArrears flags a payment failure. Escalation to throttled and
// suspended is time-based and owned by billing-service (P1.5).
func (h *handler) markAccountArrears(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return nil
	}
	state := &store.AccountQuotaState{
		AccountUUID:   userID,
		ThrottleState: "normal",
		SuspendState:  "active",
		EffectiveAt:   time.Now().UTC(),
	}
	if existing, err := h.store.GetAccountQuotaState(ctx, userID); err == nil && existing != nil {
		state = existing
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
	return h.resetQuotaForPlan(ctx, userID, plan)
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

// provisionTrialEntitlements applies the TRIAL-7D catalog entitlements right
// after a trial subscription is created (registration / OAuth first login).
// Missing catalog rows are non-fatal: the trial subscription still exists.
func (h *handler) provisionTrialEntitlements(ctx context.Context, userID string) {
	plan, err := h.store.GetBillingPlan(ctx, store.BillingPlanTrial7D)
	if err != nil {
		if !errors.Is(err, store.ErrBillingPlanNotFound) {
			slog.Warn("failed to load trial plan for entitlements", "err", err, "userID", userID)
		}
		return
	}
	if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
		slog.Warn("failed to apply trial entitlements", "err", err, "userID", userID)
		return
	}
	if err := h.resetQuotaForPlan(ctx, userID, plan); err != nil {
		slog.Warn("failed to reset trial quota", "err", err, "userID", userID)
		return
	}
	h.publishBillingEvent(ctx, &store.BillingEvent{
		Type: "trial_provisioned", UserID: userID, PlanID: plan.PlanID,
	})
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
