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

const defaultFreeQuotaBytes int64 = 5 * 1024 * 1024 * 1024

// subscriptionGracePeriod is the explicit access grace for failed payments
// before an account is moved to Free.
const subscriptionGracePeriod = 14 * 24 * time.Hour

// Missing Stripe period metadata gets one bounded retry window from the first
// persisted subscription observation; webhook updates cannot extend it.
const missingStripePeriodRetryWindow = 24 * time.Hour

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

// provisionFreeEntitlement is used only after a new account has been created.
// Read models must not call it: existing accounts without an assignment are
// intentionally left for an administrator to classify.
func (h *handler) provisionFreeEntitlement(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return nil
	}

	profile, err := h.store.GetAccountBillingProfile(ctx, userID)
	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return err
	}
	quota, err := h.store.GetAccountQuotaState(ctx, userID)
	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return err
	}
	if profile != nil && quota != nil {
		return nil
	}

	plan, err := h.store.GetBillingPlan(ctx, store.BillingPlanFree)
	if err != nil {
		if !errors.Is(err, store.ErrBillingPlanNotFound) {
			return err
		}
		// Keep newly-created Free accounts enforceable even while the catalog
		// migration is rolling out. An operator-created FREE plan still wins.
		plan = &store.BillingPlan{
			PlanID:             store.BillingPlanFree,
			PackageName:        "default",
			IncludedQuotaBytes: defaultFreeQuotaBytes,
			Active:             true,
		}
	}
	if !plan.Active {
		return nil
	}
	if profile == nil {
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
	}
	if quota != nil {
		return nil
	}
	periodStart, periodEnd := naturalMonthPeriod(time.Now())
	return h.resetQuotaForPlan(ctx, userID, plan, periodStart, periodEnd)
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

// resetQuotaForPlanOnce avoids granting the same billing period twice when
// Stripe retries an invoice event or emits more than one event for a plan
// change. A genuinely new period still receives its allowance.
func (h *handler) resetQuotaForPlanOnce(ctx context.Context, userID string, plan *store.BillingPlan, start, end time.Time) error {
	if state, err := h.store.GetAccountQuotaState(ctx, userID); err == nil && state != nil && state.PeriodStart != nil && state.PeriodEnd != nil && state.PeriodStart.Equal(start.UTC()) && state.PeriodEnd.Equal(end.UTC()) {
		if !state.Arrears && state.ArrearsSince == nil && state.ThrottleState == "normal" && state.SuspendState == "active" {
			return nil
		}
		state.Arrears = false
		state.ArrearsSince = nil
		state.ThrottleState = "normal"
		state.SuspendState = "active"
		state.EffectiveAt = time.Now().UTC()
		return h.store.UpsertAccountQuotaState(ctx, state)
	} else if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return err
	}
	return h.resetQuotaForPlan(ctx, userID, plan, start, end)
}

// applyPlanUpgradeQuota grants only the increase in the plan's monthly
// allowance, preserving usage already consumed in the current period.
func (h *handler) applyPlanUpgradeQuota(ctx context.Context, userID string, previousIncluded, upgradedIncluded int64, start, end time.Time) error {
	profile, profileErr := h.store.GetAccountBillingProfile(ctx, userID)
	if profileErr != nil && !errors.Is(profileErr, store.ErrUserNotFound) {
		return profileErr
	}
	if profile != nil && profile.IncludedQuotaBytes >= upgradedIncluded {
		return nil // The plan projection is the durable replay marker for this upgrade.
	}
	state, err := h.store.GetAccountQuotaState(ctx, userID)
	if err != nil {
		if !errors.Is(err, store.ErrUserNotFound) {
			return err
		}
		state = &store.AccountQuotaState{AccountUUID: userID}
	}
	if state == nil {
		state = &store.AccountQuotaState{AccountUUID: userID}
	}
	if state.PeriodStart == nil || state.PeriodEnd == nil {
		state.RemainingIncludedQuota = upgradedIncluded
		periodStart, periodEnd := start.UTC(), end.UTC()
		state.PeriodStart, state.PeriodEnd = &periodStart, &periodEnd
		state.EffectiveAt = time.Now().UTC()
		return h.store.UpsertAccountQuotaState(ctx, state)
	}
	if !state.PeriodStart.Equal(start.UTC()) || !state.PeriodEnd.Equal(end.UTC()) {
		currentMonthStart, currentMonthEnd := naturalMonthPeriod(time.Now().UTC())
		if !state.PeriodStart.Equal(currentMonthStart) || !state.PeriodEnd.Equal(currentMonthEnd) {
			return h.resetQuotaForPlan(ctx, userID, &store.BillingPlan{IncludedQuotaBytes: upgradedIncluded}, start, end)
		}
	}
	if state.RemainingIncludedQuota > previousIncluded {
		return nil // A concurrent delivery already applied this plan increase.
	}
	delta := upgradedIncluded - previousIncluded
	if delta <= 0 {
		return nil
	}
	state.RemainingIncludedQuota += delta
	periodStart, periodEnd := start.UTC(), end.UTC()
	state.PeriodStart, state.PeriodEnd = &periodStart, &periodEnd
	state.EffectiveAt = time.Now().UTC()
	return h.store.UpsertAccountQuotaState(ctx, state)
}

// restoreQuotaGroupForPlan reverses only a prior Free quota-group downgrade;
// unrelated operator-managed groups are preserved.
func (h *handler) restoreQuotaGroupForPlan(ctx context.Context, userID string, plan *store.BillingPlan) error {
	if plan == nil || plan.IncludedQuotaBytes <= defaultFreeQuotaBytes {
		return nil
	}
	user, err := h.store.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if store.MonthlyQuotaGroup(user) != store.MonthlyFreeQuotaLimitGroup {
		return nil
	}
	groups := make([]string, 0, len(user.Groups)+1)
	for _, group := range user.Groups {
		if group == store.MonthlyFreeQuotaLimitGroup || group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup {
			continue
		}
		groups = append(groups, group)
	}
	groups = append(groups, store.MonthlyPlusQuotaLimitGroup)
	user.Groups = normalizeGroups(groups)
	return h.store.UpdateUser(ctx, user)
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
		plan = &store.BillingPlan{
			PlanID:             store.BillingPlanFree,
			PackageName:        "default",
			IncludedQuotaBytes: defaultFreeQuotaBytes,
			Active:             true,
		}
	}
	existingProfile, profileErr := h.store.GetAccountBillingProfile(ctx, userID)
	if profileErr != nil && !errors.Is(profileErr, store.ErrUserNotFound) {
		return profileErr
	}
	existingQuota, quotaErr := h.store.GetAccountQuotaState(ctx, userID)
	if quotaErr != nil && !errors.Is(quotaErr, store.ErrUserNotFound) {
		return quotaErr
	}
	periodStart, periodEnd := naturalMonthPeriod(time.Now().UTC())
	freePackage := strings.TrimSpace(plan.PackageName)
	if freePackage == "" {
		freePackage = "default"
	}
	if existingProfile != nil && existingProfile.PricingRuleVersion == fmt.Sprintf("plan:%s", store.BillingPlanFree) && existingProfile.PackageName == freePackage && existingProfile.IncludedQuotaBytes == plan.IncludedQuotaBytes {
		if existingQuota != nil && existingQuota.PeriodStart != nil && existingQuota.PeriodEnd != nil && existingQuota.PeriodStart.Equal(periodStart) && existingQuota.PeriodEnd.Equal(periodEnd) {
			return nil
		}
	}
	remaining := plan.IncludedQuotaBytes
	if existingProfile != nil && existingQuota != nil {
		alreadyFree := existingProfile.PricingRuleVersion == fmt.Sprintf("plan:%s", store.BillingPlanFree) && existingQuota.PeriodStart != nil && existingQuota.PeriodEnd != nil && !existingQuota.PeriodEnd.After(periodStart)
		if !alreadyFree {
			used := existingProfile.IncludedQuotaBytes - existingQuota.RemainingIncludedQuota
			if used < 0 {
				used = 0
			}
			remaining = plan.IncludedQuotaBytes - used
		}
	} else if existingQuota != nil && existingQuota.RemainingIncludedQuota < remaining {
		// Without an old profile, retain the more restrictive recorded balance;
		// never turn missing snapshot data into a fresh monthly grant.
		remaining = existingQuota.RemainingIncludedQuota
	}
	if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
		return err
	}
	state := existingQuota
	if state == nil {
		state = &store.AccountQuotaState{AccountUUID: userID}
	}
	state.RemainingIncludedQuota = remaining
	state.Arrears = false
	state.ArrearsSince = nil
	state.ThrottleState = "normal"
	// A plan downgrade does not settle an explicit billing suspension.
	if state.SuspendState != "suspended" {
		state.SuspendState = "active"
	}
	state.PeriodStart = &periodStart
	state.PeriodEnd = &periodEnd
	state.EffectiveAt = time.Now().UTC()
	return h.store.UpsertAccountQuotaState(ctx, state)
}

// revokeRefundedSubscription drops paid access only when the explicitly
// refunded subscription is the user's last active Stripe subscription.
func (h *handler) revokeRefundedSubscription(ctx context.Context, userID, refundedID string) error {
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		return err
	}
	found := false
	for i := range subscriptions {
		sub := &subscriptions[i]
		if strings.TrimSpace(sub.ExternalID) != strings.TrimSpace(refundedID) {
			continue
		}
		found = true
		sub.Status = "refunded"
		if err := h.store.UpsertSubscription(ctx, sub); err != nil {
			return err
		}
		break
	}
	if !found {
		return nil
	}
	if user, err := h.store.GetUserByID(ctx, userID); err == nil && subscriptionValidityActive(user, time.Now().UTC()) {
		return nil
	} else if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return err
	}
	quota, quotaErr := h.store.GetAccountQuotaState(ctx, userID)
	if quotaErr != nil && !errors.Is(quotaErr, store.ErrUserNotFound) {
		return quotaErr
	}
	graceActive := quota != nil && quota.Arrears && quota.ArrearsSince != nil && time.Now().UTC().Before(quota.ArrearsSince.Add(subscriptionGracePeriod))
	for i := range subscriptions {
		sub := &subscriptions[i]
		if !strings.EqualFold(sub.Provider, "stripe") || !strings.EqualFold(sub.Kind, "subscription") || strings.TrimSpace(sub.ExternalID) == strings.TrimSpace(refundedID) {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(sub.Status))
		if status == "past_due" {
			if quota == nil || !quota.Arrears || quota.ArrearsSince == nil || graceActive {
				return nil
			}
			continue
		}
		if status == "active" || status == "trialing" || status == "canceling" {
			periodEnd := subscriptionMetaTime(sub.Meta, "expiresAt")
			if periodEnd.IsZero() || time.Now().UTC().Before(periodEnd) || graceActive {
				return nil
			}
		}
	}
	return h.downgradeToFreePlan(ctx, userID)
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
