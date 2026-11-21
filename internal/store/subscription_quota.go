package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrSubscriptionQuotaConflict = errors.New("subscription quota or period requires reconciliation")

// The quota delta and its replay marker are committed together. Within a paid
// period usage is preserved; a verified later paid period gets a fresh grant.
func (s *memoryStore) UpgradeSubscriptionQuota(_ context.Context, id string, previous, target int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(id, previous, target, 0, 0, start, end, false, false)
}

// First payment starts a fresh paid allowance. Existing paid upgrades retain usage.
func (s *memoryStore) ActivateSubscriptionQuota(_ context.Context, id string, previous, target int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(id, previous, target, 0, 0, start, end, true, false)
}

func (s *memoryStore) UpgradeSubscriptionDualQuota(_ context.Context, id string, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(id, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed, start, end, false, true)
}

func (s *memoryStore) ActivateSubscriptionDualQuota(_ context.Context, id string, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(id, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed, start, end, true, true)
}

func (s *memoryStore) changeSubscriptionQuota(id string, previous, target, prevHS, targetHS int64, start, end time.Time, activation, dual bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, q := s.accountBillingProfiles[id], s.accountQuotaStates[id]
	if p == nil {
		return ErrSubscriptionQuotaConflict
	}
	if p.IncludedQuotaBytes == target && (!dual || p.HighSpeedQuotaBytes == targetHS) {
		return nil
	}
	if activation && p.PricingRuleVersion != "plan:FREE" {
		return ErrSubscriptionQuotaConflict
	}
	month := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	restoringFree := p.PricingRuleVersion == "plan:FREE" && q != nil && q.PeriodStart != nil && q.PeriodEnd != nil && q.PeriodStart.Equal(month) && q.PeriodEnd.Equal(month.AddDate(0, 1, 0))
	increased := target > previous || (dual && targetHS > prevHS)
	if !increased || target < previous || (dual && targetHS < prevHS) || !end.After(start) || p.IncludedQuotaBytes != previous || (dual && p.HighSpeedQuotaBytes != prevHS) || q == nil || q.RemainingIncludedQuota > previous || (dual && prevHS > 0 && q.RemainingHighSpeedQuota > prevHS) {
		return ErrSubscriptionQuotaConflict
	}
	resetPeriod := q.PeriodStart != nil && q.PeriodEnd != nil && start.Equal(month) && end.Equal(month.AddDate(0, 1, 0)) && q.PeriodStart.After(start) && q.PeriodStart.Before(end) && q.PeriodEnd.Equal(end)
	rollover := q.PeriodStart != nil && q.PeriodEnd != nil && q.PeriodEnd.After(*q.PeriodStart) && !q.PeriodEnd.After(start)
	if !activation && !resetPeriod && !rollover && (q.PeriodStart == nil || q.PeriodEnd == nil || ((!q.PeriodStart.Equal(start) || !q.PeriodEnd.Equal(end)) && !restoringFree)) {
		return ErrSubscriptionQuotaConflict
	}
	q.RemainingIncludedQuota += target - previous
	if dual {
		q.RemainingHighSpeedQuota += targetHS - prevHS
		if q.RemainingHighSpeedQuota < 0 {
			q.RemainingHighSpeedQuota = 0
		}
	}
	if activation || rollover {
		q.RemainingIncludedQuota = target
		if dual {
			q.RemainingHighSpeedQuota = targetHS
		}
		q.Arrears, q.ArrearsSince = false, nil
		q.ThrottleState, q.SuspendState = "normal", "active"
	}
	if dual && targetHS == 0 {
		q.RemainingHighSpeedQuota = 0
	}
	if !activation && q.PeriodStart != nil && !q.PeriodStart.Before(start) && q.PeriodStart.Before(end) && q.PeriodEnd != nil && q.PeriodEnd.Equal(end) {
		start = *q.PeriodStart
	}
	q.PeriodStart, q.PeriodEnd = &start, &end
	q.UpdatedAt = time.Now().UTC()
	q.EffectiveAt = q.UpdatedAt
	p.IncludedQuotaBytes = target
	if dual {
		p.HighSpeedQuotaBytes = targetHS
	}
	p.UpdatedAt = q.UpdatedAt
	return nil
}

func (s *postgresStore) UpgradeSubscriptionQuota(ctx context.Context, id string, previous, target int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(ctx, id, previous, target, 0, 0, start, end, false, false)
}

func (s *postgresStore) ActivateSubscriptionQuota(ctx context.Context, id string, previous, target int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(ctx, id, previous, target, 0, 0, start, end, true, false)
}

func (s *postgresStore) UpgradeSubscriptionDualQuota(ctx context.Context, id string, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(ctx, id, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed, start, end, false, true)
}

func (s *postgresStore) ActivateSubscriptionDualQuota(ctx context.Context, id string, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed int64, start, end time.Time) error {
	return s.changeSubscriptionQuota(ctx, id, previousIncluded, targetIncluded, previousHighSpeed, targetHighSpeed, start, end, true, true)
}

func (s *postgresStore) changeSubscriptionQuota(ctx context.Context, id string, previous, target, prevHS, targetHS int64, start, end time.Time, activation, dual bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var included, highSpeed int64
	var rule string
	if dual {
		if err = tx.QueryRowContext(ctx, `SELECT included_quota_bytes,high_speed_quota_bytes,pricing_rule_version FROM public.account_billing_profiles WHERE account_uuid=$1 FOR UPDATE`, id).Scan(&included, &highSpeed, &rule); err != nil {
			return err
		}
	} else {
		if err = tx.QueryRowContext(ctx, `SELECT included_quota_bytes,pricing_rule_version FROM public.account_billing_profiles WHERE account_uuid=$1 FOR UPDATE`, id).Scan(&included, &rule); err != nil {
			return err
		}
	}
	if included == target && (!dual || highSpeed == targetHS) {
		return tx.Commit()
	}
	if activation && rule != "plan:FREE" {
		return ErrSubscriptionQuotaConflict
	}
	increased := target > previous || (dual && targetHS > prevHS)
	if !increased || target < previous || (dual && targetHS < prevHS) || !end.After(start) || included != previous || (dual && highSpeed != prevHS) {
		return ErrSubscriptionQuotaConflict
	}
	month := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	var result sql.Result
	if dual {
		result, err = tx.ExecContext(ctx, `UPDATE public.account_quota_states SET
		 remaining_included_quota=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN $10 ELSE remaining_included_quota+$2 END,
		 remaining_high_speed_quota=CASE WHEN $12 = 0 THEN 0 WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN $12 ELSE GREATEST(0, remaining_high_speed_quota+$11) END,
		 arrears=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN false ELSE arrears END,
		 arrears_since=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN NULL ELSE arrears_since END,
		 throttle_state=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN 'normal' ELSE throttle_state END,
		 suspend_state=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN 'active' ELSE suspend_state END,
		 period_start=CASE WHEN NOT $9 AND period_start >= $3 AND period_start < $4 AND period_end=$4 THEN period_start ELSE $3 END,period_end=$4,effective_at=now(),updated_at=now()
		 WHERE account_uuid=$1 AND ($9 OR (period_end>period_start AND period_end<=$3) OR ((period_start=$3 AND period_end=$4) OR ($3=$7 AND $4=$8 AND period_start>$3 AND period_start<$4 AND period_end=$4)) OR ($6 AND period_start=$7 AND period_end=$8)) AND remaining_included_quota<=$5 AND ($13 <= 0 OR remaining_high_speed_quota<=$13)`, id, target-previous, start.UTC(), end.UTC(), previous, rule == "plan:FREE", month, month.AddDate(0, 1, 0), activation, target, targetHS-prevHS, targetHS, prevHS)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE public.account_quota_states SET
		 remaining_included_quota=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN $10 ELSE remaining_included_quota+$2 END,
		 arrears=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN false ELSE arrears END,
		 arrears_since=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN NULL ELSE arrears_since END,
		 throttle_state=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN 'normal' ELSE throttle_state END,
		 suspend_state=CASE WHEN $9 OR (period_end>period_start AND period_end<=$3) THEN 'active' ELSE suspend_state END,
		 period_start=CASE WHEN NOT $9 AND period_start >= $3 AND period_start < $4 AND period_end=$4 THEN period_start ELSE $3 END,period_end=$4,effective_at=now(),updated_at=now()
		 WHERE account_uuid=$1 AND ($9 OR (period_end>period_start AND period_end<=$3) OR ((period_start=$3 AND period_end=$4) OR ($3=$7 AND $4=$8 AND period_start>$3 AND period_start<$4 AND period_end=$4)) OR ($6 AND period_start=$7 AND period_end=$8)) AND remaining_included_quota<=$5`, id, target-previous, start.UTC(), end.UTC(), previous, rule == "plan:FREE", month, month.AddDate(0, 1, 0), activation, target)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSubscriptionQuotaConflict
	}
	if dual {
		if _, err = tx.ExecContext(ctx, `UPDATE public.account_billing_profiles SET included_quota_bytes=$2,high_speed_quota_bytes=$3,updated_at=now() WHERE account_uuid=$1`, id, target, targetHS); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE public.account_billing_profiles SET included_quota_bytes=$2,updated_at=now() WHERE account_uuid=$1`, id, target); err != nil {
			return err
		}
	}
	return tx.Commit()
}
