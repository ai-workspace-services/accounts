package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"time"
)

const AuditActionMonthlyUsageReset = "admin.monthly_usage.reset"

// A minute boundary matches the immutable accounting buckets. Historical buckets
// remain available; quota recomputation starts after the last completed bucket.
func resetUsagePeriod(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	start := now.Truncate(time.Minute).Add(time.Minute)
	end := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return start, end
}

func (s *postgresStore) ResetMonthlyUsage(ctx context.Context, id, actor, email, reason string, now time.Time) (*AccountQuotaState, error) {
	start, end := resetUsagePeriod(now)
	if !start.Before(end) {
		return nil, errors.New("retry reset in the next UTC month")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", id); err != nil {
		return nil, err
	}
	var included, remaining int64
	var currentEmail string
	if err = tx.QueryRowContext(ctx, `SELECT p.included_quota_bytes,q.remaining_included_quota,u.email FROM account_billing_profiles p JOIN account_quota_states q USING(account_uuid) JOIN users u ON u.uuid::text=q.account_uuid::text WHERE q.account_uuid=$1 FOR UPDATE OF p,q,u`, id).Scan(&included, &remaining, &currentEmail); err != nil {
		return nil, err
	}
	if currentEmail != email || included <= 0 {
		return nil, errors.New("account email changed or no monthly quota")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE account_quota_states SET remaining_included_quota=$2,period_start=$3,period_end=$4,effective_at=$5,updated_at=$5 WHERE account_uuid=$1`, id, included, start, end, now.UTC()); err != nil {
		return nil, err
	}
	details, _ := json.Marshal(map[string]any{"target_uuid": id, "email": email, "reason": reason, "before": map[string]any{"remaining_included_quota": remaining}, "after": map[string]any{"remaining_included_quota": included, "period_start": start, "period_end": end}, "expires_at": end})
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_logs(uuid,action,actor_uuid,details,created_at) VALUES($1,$2,$3,$4,$5)`, uuid.NewString(), AuditActionMonthlyUsageReset, actor, details, now.UTC()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAccountQuotaState(ctx, id)
}

func (s *memoryStore) ResetMonthlyUsage(ctx context.Context, id, actor, email, reason string, now time.Time) (*AccountQuotaState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start, end := resetUsagePeriod(now)
	u, p, q := s.byID[id], s.accountBillingProfiles[id], s.accountQuotaStates[id]
	if !start.Before(end) || u == nil || u.Email != email || p == nil || p.IncludedQuotaBytes <= 0 || q == nil {
		return nil, errors.New("account email changed or no monthly quota")
	}
	before := q.RemainingIncludedQuota
	q.RemainingIncludedQuota = p.IncludedQuotaBytes
	q.PeriodStart = &start
	q.PeriodEnd = &end
	q.EffectiveAt = now.UTC()
	q.UpdatedAt = now.UTC()
	s.auditLogs = append(s.auditLogs, &AuditLog{UUID: uuid.NewString(), ActorUUID: actor, Action: AuditActionMonthlyUsageReset, CreatedAt: now.UTC(), Details: map[string]any{"target_uuid": id, "email": email, "reason": reason, "before": map[string]any{"remaining_included_quota": before}, "after": map[string]any{"remaining_included_quota": q.RemainingIncludedQuota, "period_start": start, "period_end": end}, "expires_at": end}})
	return cloneQuotaState(q), nil
}

func (s *memoryStore) expireMonthlyUsageResetLocked(id string, now time.Time) {
	q, p := s.accountQuotaStates[id], s.accountBillingProfiles[id]
	if q == nil || p == nil || q.PeriodEnd == nil || q.PeriodStart == nil || q.PeriodEnd.After(now) {
		return
	}
	matched := false
	for _, a := range s.auditLogs {
		if a.Action != AuditActionMonthlyUsageReset || a.Details["target_uuid"] != id {
			continue
		}
		after, ok := a.Details["after"].(map[string]any)
		if !ok {
			continue
		}
		start, ok1 := after["period_start"].(time.Time)
		end, ok2 := after["period_end"].(time.Time)
		if ok1 && ok2 && start.Equal(*q.PeriodStart) && end.Equal(*q.PeriodEnd) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	var used int64
	for _, b := range s.trafficMinuteBuckets {
		if b.AccountUUID == id && !b.BucketStart.Before(start) && b.BucketStart.Before(end) {
			used += max(int64(0), b.TotalBytes)
		}
	}
	q.RemainingIncludedQuota = max(int64(0), p.IncludedQuotaBytes-used)
	q.PeriodStart = &start
	q.PeriodEnd = &end
	q.EffectiveAt = now
	q.UpdatedAt = now
}
