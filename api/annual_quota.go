package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"account/internal/store"
)

// subscriptionQuotaPeriod keeps annual commercial billing separate from the
// monthly allowance advertised by annual plans. Stripe renews an annual Price
// once a year, but the included quota is a monthly grant, not a yearly pool.
func subscriptionQuotaPeriod(plan *store.BillingPlan, subscription *stripeSubscription, now time.Time) (time.Time, time.Time) {
	if plan != nil && strings.EqualFold(strings.TrimSpace(plan.PriceUnit), "year") {
		return naturalMonthPeriod(now)
	}
	return subscriptionPeriod(subscription)
}

// ReconcileAnnualPlanQuotas grants the new natural-month allowance for active
// annual subscriptions. It is safe to run repeatedly: the stored period_end
// is the idempotency boundary.
func ReconcileAnnualPlanQuotas(ctx context.Context, st store.Store, now time.Time) (int, error) {
	users, err := st.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	start, end := naturalMonthPeriod(now)
	reset := 0
	for _, user := range users {
		subs, err := st.ListSubscriptionsByUser(ctx, user.ID)
		if err != nil {
			return reset, err
		}
		for i := range subs {
			sub := subs[i]
			if !strings.EqualFold(sub.Provider, "stripe") || !strings.EqualFold(sub.Kind, "subscription") || !strings.EqualFold(sub.Status, "active") {
				continue
			}
			plan, err := st.GetBillingPlan(ctx, sub.PlanID)
			if err != nil || !strings.EqualFold(strings.TrimSpace(plan.PriceUnit), "year") {
				continue
			}
			quota, err := st.GetAccountQuotaState(ctx, user.ID)
			if err == nil && quota != nil && quota.PeriodEnd != nil && quota.PeriodEnd.After(now.UTC()) {
				break
			}
			state := &store.AccountQuotaState{AccountUUID: user.ID}
			if quota != nil {
				state = quota
			}
			state.RemainingIncludedQuota = plan.IncludedQuotaBytes
			state.PeriodStart = &start
			state.PeriodEnd = &end
			state.EffectiveAt = now.UTC()
			if err := st.UpsertAccountQuotaState(ctx, state); err != nil {
				return reset, err
			}
			reset++
			break
		}
	}
	return reset, nil
}

// StartAnnualQuotaReconciler immediately repairs overdue grants and then
// checks hourly. The operation itself is idempotent, so multiple replicas are
// safe and do not multiply quota.
func StartAnnualQuotaReconciler(ctx context.Context, st store.Store, logger *slog.Logger) {
	run := func() {
		count, err := ReconcileAnnualPlanQuotas(context.Background(), st, time.Now().UTC())
		if err != nil {
			logger.Warn("annual quota reconciliation failed", "err", err)
		} else if count > 0 {
			logger.Info("annual monthly quotas reset", "accounts", count)
		}
	}
	run()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
