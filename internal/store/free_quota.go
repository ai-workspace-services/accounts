package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ResetFreeQuotaPeriod advances the natural-month allowance exclusively for
// configurable whitelist free group members (`segment:whitelist:free`), reading
// the group's live configured monthly quota from billing_plans (WHITELIST-FREE).
// If the account was paused due to quota exhaustion in the prior period and the
// new period leaves positive remaining quota, proxy_access_state is restored to 'active'.
func (s *postgresStore) ResetFreeQuotaPeriod(ctx context.Context, id string, start, end, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var configuredQuota int64
	err = tx.QueryRowContext(ctx, `SELECT included_quota_bytes FROM public.billing_plans WHERE plan_id = $1 AND active`, BillingPlanWhitelistFree).Scan(&configuredQuota)
	if errors.Is(err, sql.ErrNoRows) {
		configuredQuota = 0
	} else if err != nil {
		return err
	}

	if configuredQuota > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE public.account_billing_profiles p
SET included_quota_bytes = $2, updated_at = $3
FROM public.users u
WHERE p.account_uuid = $1 AND u.uuid = p.account_uuid
AND u.groups ? $4 AND p.pricing_rule_version = $5
AND p.included_quota_bytes <> $2`, id, configuredQuota, now.UTC(), WhitelistFreeQuotaGroup, "plan:"+BillingPlanWhitelistFree); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE public.account_quota_states q
SET remaining_included_quota = GREATEST(0, p.included_quota_bytes - COALESCE((SELECT SUM(GREATEST(total_bytes, 0)) FROM public.traffic_minute_buckets b WHERE b.account_uuid = q.account_uuid AND b.bucket_start >= $2 AND b.bucket_start < $3), 0)),
    proxy_access_state = CASE
        WHEN q.remaining_included_quota <= 0 AND q.proxy_access_state = 'paused'
             AND GREATEST(0, p.included_quota_bytes - COALESCE((SELECT SUM(GREATEST(total_bytes, 0)) FROM public.traffic_minute_buckets b WHERE b.account_uuid = q.account_uuid AND b.bucket_start >= $2 AND b.bucket_start < $3), 0)) > 0
        THEN 'active'
        ELSE COALESCE(NULLIF(trim(q.proxy_access_state), ''), 'active')
    END,
    period_start = $2, period_end = $3, effective_at = $4, updated_at = $4
FROM public.account_billing_profiles p, public.users u
WHERE q.account_uuid = $1 AND p.account_uuid = q.account_uuid AND u.uuid = q.account_uuid
AND u.groups ? $5 AND p.pricing_rule_version = $6
AND (q.period_end IS NULL OR (q.period_end <= $4 AND q.period_end <= $2))`, id, start.UTC(), end.UTC(), now.UTC(), WhitelistFreeQuotaGroup, "plan:"+BillingPlanWhitelistFree); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *memoryStore) ResetFreeQuotaPeriod(ctx context.Context, id string, start, end, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byID[id]
	p := s.accountBillingProfiles[id]
	q := s.accountQuotaStates[id]
	if u == nil || !IsMonthlyFreeQuotaLimitMember(u) || p == nil || p.PricingRuleVersion != "plan:"+BillingPlanWhitelistFree || q == nil || (q.PeriodEnd != nil && (q.PeriodEnd.After(start) || q.PeriodEnd.After(now))) {
		return nil
	}
	if plan, ok := s.billingPlans[BillingPlanWhitelistFree]; ok && plan != nil && plan.Active && plan.IncludedQuotaBytes > 0 {
		p.IncludedQuotaBytes = plan.IncludedQuotaBytes
	}
	var used int64
	for _, b := range s.trafficMinuteBuckets {
		if b.AccountUUID == id && !b.BucketStart.Before(start) && b.BucketStart.Before(end) && b.TotalBytes > 0 {
			used += b.TotalBytes
		}
	}
	wasExhaustedPaused := q.RemainingIncludedQuota <= 0 && q.ProxyAccessState == "paused"
	q.RemainingIncludedQuota = p.IncludedQuotaBytes - used
	if q.RemainingIncludedQuota < 0 {
		q.RemainingIncludedQuota = 0
	}
	if wasExhaustedPaused && q.RemainingIncludedQuota > 0 {
		q.ProxyAccessState = "active"
	} else if q.ProxyAccessState == "" {
		q.ProxyAccessState = "active"
	}
	q.PeriodStart = &start
	q.PeriodEnd = &end
	q.EffectiveAt = now
	q.UpdatedAt = now
	return nil
}
