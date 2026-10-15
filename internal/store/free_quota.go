package store

import (
	"context"
	"time"
)

func (s *postgresStore) ResetFreeQuotaPeriod(ctx context.Context, id string, start, end, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE public.account_quota_states q
SET remaining_included_quota=GREATEST(0,p.included_quota_bytes-COALESCE((SELECT SUM(total_bytes) FROM public.traffic_minute_buckets b WHERE b.account_uuid=q.account_uuid AND b.bucket_start >= $2 AND b.bucket_start < $3),0)),
period_start=$2, period_end=$3, effective_at=$4, updated_at=$4
FROM public.account_billing_profiles p, public.users u
WHERE q.account_uuid=$1 AND p.account_uuid=q.account_uuid AND u.uuid=q.account_uuid
AND u.groups ? 'segment:quota:free-3gb' AND p.pricing_rule_version='plan:FREE'
AND (q.period_end IS NULL OR (q.period_end <= $4 AND q.period_end <= $2))`, id, start.UTC(), end.UTC(), now.UTC())
	return err
}

func (s *memoryStore) ResetFreeQuotaPeriod(ctx context.Context, id string, start, end, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byID[id]
	p := s.accountBillingProfiles[id]
	q := s.accountQuotaStates[id]
	if u == nil || !IsMonthlyFreeQuotaLimitMember(u) || p == nil || p.PricingRuleVersion != "plan:FREE" || q == nil || q.PeriodEnd != nil && (q.PeriodEnd.After(start) || q.PeriodEnd.After(now)) {
		return nil
	}
	var used int64
	for _, b := range s.trafficMinuteBuckets {
		if b.AccountUUID == id && !b.BucketStart.Before(start) && b.BucketStart.Before(end) {
			used += b.TotalBytes
		}
	}
	q.RemainingIncludedQuota = p.IncludedQuotaBytes - used
	if q.RemainingIncludedQuota < 0 {
		q.RemainingIncludedQuota = 0
	}
	q.PeriodStart = &start
	q.PeriodEnd = &end
	q.EffectiveAt = now
	q.UpdatedAt = now
	return nil
}
