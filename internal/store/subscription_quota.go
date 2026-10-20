package store

import (
	"context"
	"errors"
	"time"
)

var ErrSubscriptionQuotaConflict = errors.New("subscription quota or period requires reconciliation")

// The quota delta and its replay marker are committed together. Never refill
// an absent grant, change a period, or overwrite a concurrent usage deduction.
func (s *memoryStore) UpgradeSubscriptionQuota(_ context.Context, id string, previous, target int64, start, end time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, q := s.accountBillingProfiles[id], s.accountQuotaStates[id]
	if p == nil {
		return ErrSubscriptionQuotaConflict
	}
	if p.IncludedQuotaBytes == target {
		return nil
	}
	month := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	restoringFree := p.PricingRuleVersion == "plan:FREE" && q != nil && q.PeriodStart != nil && q.PeriodEnd != nil && q.PeriodStart.Equal(month) && q.PeriodEnd.Equal(month.AddDate(0, 1, 0))
	if target <= previous || p.IncludedQuotaBytes != previous || q == nil || q.PeriodStart == nil || q.PeriodEnd == nil || ((!q.PeriodStart.Equal(start) || !q.PeriodEnd.Equal(end)) && !restoringFree) || q.RemainingIncludedQuota > previous {
		return ErrSubscriptionQuotaConflict
	}
	q.RemainingIncludedQuota += target - previous
	q.PeriodStart, q.PeriodEnd = &start, &end
	q.UpdatedAt = time.Now().UTC()
	q.EffectiveAt = q.UpdatedAt
	p.IncludedQuotaBytes = target
	p.UpdatedAt = q.UpdatedAt
	return nil
}

func (s *postgresStore) UpgradeSubscriptionQuota(ctx context.Context, id string, previous, target int64, start, end time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var included int64
	var rule string
	if err = tx.QueryRowContext(ctx, `SELECT included_quota_bytes,pricing_rule_version FROM public.account_billing_profiles WHERE account_uuid=$1 FOR UPDATE`, id).Scan(&included, &rule); err != nil {
		return err
	}
	if included == target {
		return tx.Commit()
	}
	if target <= previous || included != previous {
		return ErrSubscriptionQuotaConflict
	}
	month := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	result, err := tx.ExecContext(ctx, `UPDATE public.account_quota_states SET remaining_included_quota=remaining_included_quota+$2,period_start=$3,period_end=$4,effective_at=now(),updated_at=now() WHERE account_uuid=$1 AND ((period_start=$3 AND period_end=$4) OR ($6 AND period_start=$7 AND period_end=$8)) AND remaining_included_quota<=$5`, id, target-previous, start.UTC(), end.UTC(), previous, rule == "plan:FREE", month, month.AddDate(0, 1, 0))
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
	if _, err = tx.ExecContext(ctx, `UPDATE public.account_billing_profiles SET included_quota_bytes=$2,updated_at=now() WHERE account_uuid=$1`, id, target); err != nil {
		return err
	}
	return tx.Commit()
}
