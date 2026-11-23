package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"account/internal/store"
)

const defaultFreeQuotaBytes int64 = 5 * 1024 * 1024 * 1024

// defaultSubscriptionGracePeriod is the explicit 3-day access grace for failed
// payments before an account is moved to Free.
const defaultSubscriptionGracePeriod = 3 * 24 * time.Hour

func subscriptionGracePeriod() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SUBSCRIPTION_GRACE_PERIOD"))
	if raw == "" {
		return defaultSubscriptionGracePeriod
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return defaultSubscriptionGracePeriod
	}
	return parsed
}

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
		AccountUUID:         userID,
		PackageName:         packageName,
		IncludedQuotaBytes:  plan.IncludedQuotaBytes,
		HighSpeedQuotaBytes: plan.HighSpeedQuotaBytes,
		RegionMultiplier:    plan.Multiplier("region"),
		LineMultiplier:      plan.Multiplier("line"),
		PeakMultiplier:      plan.Multiplier("peak"),
		OffPeakMultiplier:   plan.Multiplier("offpeak"),
		PricingRuleVersion:  fmt.Sprintf("plan:%s", plan.PlanID),
	}
	// Preserve an operator-tuned base price when one exists; the catalog does
	// not own per-byte pricing yet (billing-service defaults apply otherwise).
	if existing, err := h.store.GetAccountBillingProfile(ctx, userID); err == nil && existing != nil {
		profile.BasePricePerByte = existing.BasePricePerByte
	}
	return h.store.UpsertAccountBillingProfile(ctx, profile)
}

// provisionOnboardingEntitlement initializes new accounts created via email or OAuth:
// whitelist members receive the configurable whitelist monthly free allowance
// (Media-Pool only); all other newly registered accounts receive a one-time
// 5 GiB experience quota across all public pools (Media-Pool + Pro-EC2-Pool)
// that permanently pauses traffic once exhausted until a paid subscription activates.
// Read models must not call it: existing accounts without an assignment are
// intentionally left for an administrator to classify.
func (h *handler) provisionOnboardingEntitlement(ctx context.Context, userID string) error {
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

	user, userErr := h.store.GetUserByID(ctx, userID)
	if userErr != nil && !errors.Is(userErr, store.ErrUserNotFound) {
		return userErr
	}

	if user != nil && store.IsWhitelistFreeMember(user) {
		plan := h.resolveWhitelistFreePlan(ctx)
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
		periodStart, periodEnd := naturalMonthPeriod(time.Now().UTC())
		return h.resetQuotaForPlan(ctx, userID, plan, periodStart, periodEnd)
	}

	trialPlan := &store.BillingPlan{
		PlanID:              store.BillingPlanTrialOnce5GB,
		PackageName:         "trial",
		IncludedQuotaBytes:  store.DefaultOneTimeTrialQuotaBytes,
		HighSpeedQuotaBytes: 0,
		Active:              true,
	}
	if configuredTrial, err := h.store.GetBillingPlan(ctx, store.BillingPlanTrialOnce5GB); err == nil && configuredTrial != nil && configuredTrial.Active && configuredTrial.IncludedQuotaBytes > 0 {
		trialPlan = configuredTrial
	}

	if user != nil && !store.IsOneTimeTrialMember(user) {
		groups := make([]string, 0, len(user.Groups)+1)
		for _, g := range user.Groups {
			if store.IsMonthlyFreeQuotaGroup(g) || g == "segment:free" {
				continue
			}
			groups = append(groups, g)
		}
		groups = append(groups, store.OneTimeTrialQuotaGroup)
		user.Groups = normalizeGroups(groups)
		if err := h.store.UpdateUser(ctx, user); err != nil {
			return err
		}
	}

	if profile == nil {
		if err := h.applyPlanEntitlements(ctx, userID, trialPlan); err != nil {
			return err
		}
	}
	if quota != nil {
		return nil
	}
	now := time.Now().UTC()
	state := &store.AccountQuotaState{
		AccountUUID:             userID,
		RemainingIncludedQuota:  trialPlan.IncludedQuotaBytes,
		RemainingHighSpeedQuota: 0,
		Arrears:                 false,
		ThrottleState:           "normal",
		SuspendState:            "active",
		ProxyAccessState:        "active",
		PeriodStart:             &now,
		PeriodEnd:               nil, // One-time allowance: never resets on calendar rollover.
		EffectiveAt:             now,
	}
	return h.store.UpsertAccountQuotaState(ctx, state)
}

func (h *handler) provisionFreeEntitlement(ctx context.Context, userID string) error {
	return h.provisionOnboardingEntitlement(ctx, userID)
}

func (h *handler) resolveWhitelistFreePlan(ctx context.Context) *store.BillingPlan {
	if plan, err := h.store.GetBillingPlan(ctx, store.BillingPlanWhitelistFree); err == nil && plan != nil {
		copyPlan := *plan
		if strings.TrimSpace(copyPlan.PackageName) == "" {
			copyPlan.PackageName = "whitelist"
		}
		return &copyPlan
	}
	return &store.BillingPlan{
		PlanID:              store.BillingPlanWhitelistFree,
		DisplayName:         "白名单免费组",
		Kind:                "whitelist_group",
		PackageName:         "whitelist",
		IncludedQuotaBytes:  defaultFreeQuotaBytes,
		HighSpeedQuotaBytes: 0,
		Active:              true,
	}
}

// resetQuotaForPlan re-arms the quota state for a fresh billing period
// (subscription activation or invoice.paid renewal) and clears dunning/pause flags.
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
	state.RemainingHighSpeedQuota = plan.HighSpeedQuotaBytes
	state.Arrears = false
	state.ArrearsSince = nil
	state.ThrottleState = "normal"
	state.SuspendState = "active"
	state.ProxyAccessState = "active"
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
	if state, err := h.store.GetAccountQuotaState(ctx, userID); err == nil && state != nil && state.PeriodStart != nil && state.PeriodEnd != nil && !state.PeriodStart.Before(start.UTC()) && state.PeriodStart.Before(end.UTC()) && state.PeriodEnd.Equal(end.UTC()) {
		if !state.Arrears && state.ArrearsSince == nil && state.ThrottleState == "normal" && state.SuspendState == "active" && state.ProxyAccessState == "active" {
			return nil
		}
		state.Arrears = false
		state.ArrearsSince = nil
		state.ThrottleState = "normal"
		state.SuspendState = "active"
		state.ProxyAccessState = "active"
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
	return h.store.UpgradeSubscriptionQuota(ctx, userID, previousIncluded, upgradedIncluded, start, end)
}

func (h *handler) applyPlanUpgradeDualQuota(ctx context.Context, userID string, previousIncluded, upgradedIncluded, previousHighSpeed, upgradedHighSpeed int64, start, end time.Time) error {
	return h.store.UpgradeSubscriptionDualQuota(ctx, userID, previousIncluded, upgradedIncluded, previousHighSpeed, upgradedHighSpeed, start, end)
}

func (h *handler) clampDowngradedHighSpeedQuota(ctx context.Context, userID string, plan *store.BillingPlan) error {
	if plan == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	state, err := h.store.GetAccountQuotaState(ctx, userID)
	if errors.Is(err, store.ErrUserNotFound) || state == nil {
		return nil
	}
	if err != nil {
		return err
	}
	target := max(int64(0), plan.HighSpeedQuotaBytes)
	if state.RemainingHighSpeedQuota <= target {
		return nil
	}
	state.RemainingHighSpeedQuota = target
	state.EffectiveAt = time.Now().UTC()
	return h.store.UpsertAccountQuotaState(ctx, state)
}

// restoreQuotaGroupForPlan transitions an account from trial, free, whitelist,
// or paused state into the paid monthly quota group while preserving unrelated
// operator-managed labels (including segment:whitelist:free membership).
func (h *handler) restoreQuotaGroupForPlan(ctx context.Context, userID string, plan *store.BillingPlan) error {
	if plan == nil {
		return nil
	}
	pkg := strings.ToLower(strings.TrimSpace(plan.PackageName))
	if pkg == "free" || pkg == "trial" || pkg == "whitelist" || pkg == "paused" {
		return nil
	}
	user, err := h.store.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	currentGroup := store.MonthlyQuotaGroup(user)
	if currentGroup == store.MonthlyPlusQuotaLimitGroup && !store.IsOneTimeTrialMember(user) {
		return nil
	}
	groups := make([]string, 0, len(user.Groups)+1)
	for _, group := range user.Groups {
		if store.IsMonthlyQuotaGroup(group) || group == store.OneTimeTrialQuotaGroup || group == "segment:free" {
			continue
		}
		groups = append(groups, group)
	}
	if currentGroup == store.MonthlyUnlimitedBetaQuotaGroup {
		groups = append(groups, store.MonthlyUnlimitedBetaQuotaGroup)
	} else {
		groups = append(groups, store.MonthlyPlusQuotaLimitGroup)
	}
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

// pauseTrafficOrFallbackWhitelist ends a paid subscription: whitelist free
// group members revert to their configured monthly whitelist allowance, while
// all other accounts enter permanent "paused traffic" mode (proxy_access_state='paused',
// never downgraded to Free) while keeping console login active so they can resubscribe.
func (h *handler) pauseTrafficOrFallbackWhitelist(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return nil
	}
	user, userErr := h.store.GetUserByID(ctx, userID)
	if userErr != nil && !errors.Is(userErr, store.ErrUserNotFound) {
		return userErr
	}
	isWhitelist := user != nil && store.IsWhitelistFreeMember(user)
	if user != nil {
		groups := make([]string, 0, len(user.Groups))
		for _, group := range user.Groups {
			switch group {
			case store.MonthlyFreeQuotaLimitGroup, store.MonthlyFreeQuota5GBAliasGroup,
				store.MonthlyPlusQuotaLimitGroup, store.MonthlyUnlimitedBetaQuotaGroup,
				store.SubscriptionListedGroup, store.SubscriptionUnlimitedOverrideGroup,
				store.OneTimeTrialQuotaGroup, "segment:free",
				"segment:subscribed", "segment:monthly", "segment:yearly",
				"segment:subscription:monthly", "segment:subscription:yearly":
				continue
			}
			groups = append(groups, group)
		}
		user.Groups = normalizeGroups(groups)
		if err := h.store.UpdateUser(ctx, user); err != nil {
			return err
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

	now := time.Now().UTC()
	if isWhitelist {
		plan := h.resolveWhitelistFreePlan(ctx)
		periodStart, periodEnd := naturalMonthPeriod(now)
		pkg := strings.TrimSpace(plan.PackageName)
		if pkg == "" {
			pkg = "whitelist"
		}
		if existingProfile != nil && existingProfile.PricingRuleVersion == fmt.Sprintf("plan:%s", plan.PlanID) && existingProfile.PackageName == pkg && existingProfile.IncludedQuotaBytes == plan.IncludedQuotaBytes && existingProfile.HighSpeedQuotaBytes == 0 {
			if existingQuota != nil && existingQuota.PeriodStart != nil && existingQuota.PeriodEnd != nil && existingQuota.PeriodStart.Equal(periodStart) && existingQuota.PeriodEnd.Equal(periodEnd) {
				return nil
			}
		}
		remaining := plan.IncludedQuotaBytes
		if existingProfile != nil && existingQuota != nil {
			alreadyWL := existingProfile.PricingRuleVersion == fmt.Sprintf("plan:%s", plan.PlanID) && existingQuota.PeriodStart != nil && existingQuota.PeriodEnd != nil && !existingQuota.PeriodEnd.After(periodStart)
			if !alreadyWL {
				used := existingProfile.IncludedQuotaBytes - existingQuota.RemainingIncludedQuota
				if used < 0 {
					used = 0
				}
				remaining = plan.IncludedQuotaBytes - used
				if remaining < 0 {
					remaining = 0
				}
			}
		} else if existingQuota != nil && existingQuota.RemainingIncludedQuota < remaining {
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
		state.RemainingHighSpeedQuota = 0
		state.Arrears = false
		state.ArrearsSince = nil
		state.ThrottleState = "normal"
		if state.SuspendState != "suspended" {
			state.SuspendState = "active"
		}
		if remaining > 0 {
			state.ProxyAccessState = "active"
		} else {
			state.ProxyAccessState = "paused"
		}
		state.PeriodStart = &periodStart
		state.PeriodEnd = &periodEnd
		state.EffectiveAt = now
		return h.store.UpsertAccountQuotaState(ctx, state)
	}

	if existingProfile != nil &&
		existingProfile.PricingRuleVersion == "plan:"+store.BillingPlanPaused &&
		existingProfile.PackageName == "paused" &&
		existingProfile.IncludedQuotaBytes == 0 &&
		existingProfile.HighSpeedQuotaBytes == 0 &&
		existingQuota != nil &&
		existingQuota.RemainingIncludedQuota == 0 &&
		existingQuota.RemainingHighSpeedQuota == 0 &&
		!existingQuota.Arrears &&
		existingQuota.ArrearsSince == nil &&
		existingQuota.ThrottleState == "normal" &&
		existingQuota.ProxyAccessState == "paused" &&
		existingQuota.PeriodStart == nil &&
		existingQuota.PeriodEnd == nil {
		return nil
	}

	pausedPlan := &store.BillingPlan{
		PlanID:              store.BillingPlanPaused,
		PackageName:         "paused",
		IncludedQuotaBytes:  0,
		HighSpeedQuotaBytes: 0,
		Active:              true,
	}
	if err := h.applyPlanEntitlements(ctx, userID, pausedPlan); err != nil {
		return err
	}
	state := existingQuota
	if state == nil {
		state = &store.AccountQuotaState{AccountUUID: userID}
	}
	state.RemainingIncludedQuota = 0
	state.RemainingHighSpeedQuota = 0
	state.Arrears = false
	state.ArrearsSince = nil
	state.ThrottleState = "normal"
	if state.SuspendState != "suspended" {
		state.SuspendState = "active"
	}
	state.ProxyAccessState = "paused"
	state.PeriodStart = nil
	state.PeriodEnd = nil
	state.EffectiveAt = now
	return h.store.UpsertAccountQuotaState(ctx, state)
}

func (h *handler) downgradeToFreePlan(ctx context.Context, userID string) error {
	return h.pauseTrafficOrFallbackWhitelist(ctx, userID)
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
	if user, err := h.store.GetUserByID(ctx, userID); err == nil && activeListedSubscriptionGrant(user, time.Now().UTC()) {
		return nil
	} else if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return err
	}
	quota, quotaErr := h.store.GetAccountQuotaState(ctx, userID)
	if quotaErr != nil && !errors.Is(quotaErr, store.ErrUserNotFound) {
		return quotaErr
	}
	graceActive := quota != nil && quota.Arrears && quota.ArrearsSince != nil && time.Now().UTC().Before(quota.ArrearsSince.Add(subscriptionGracePeriod()))
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

// A date left behind after an entitlement was explicitly removed is historical
// state, not a current grant. Preserve local subscription access only while the
// account still carries the authoritative subscription-list marker.
func activeListedSubscriptionGrant(user *store.User, now time.Time) bool {
	if !subscriptionValidityActive(user, now) {
		return false
	}
	for _, group := range user.Groups {
		if group == store.SubscriptionListedGroup {
			return true
		}
	}
	return false
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
