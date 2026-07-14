package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *postgresStore) UpsertTrafficStatCheckpoint(ctx context.Context, checkpoint *TrafficStatCheckpoint) error {
	if checkpoint == nil {
		return errors.New("checkpoint is required")
	}

	const query = `
		INSERT INTO traffic_stat_checkpoints (
			node_id, account_uuid, last_uplink_total, last_downlink_total, last_seen_at, xray_revision, reset_epoch
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (node_id, account_uuid) DO UPDATE SET
			last_uplink_total = EXCLUDED.last_uplink_total,
			last_downlink_total = EXCLUDED.last_downlink_total,
			last_seen_at = EXCLUDED.last_seen_at,
			xray_revision = EXCLUDED.xray_revision,
			reset_epoch = EXCLUDED.reset_epoch,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		strings.TrimSpace(checkpoint.NodeID),
		strings.TrimSpace(checkpoint.AccountUUID),
		checkpoint.LastUplinkTotal,
		checkpoint.LastDownlinkTotal,
		checkpoint.LastSeenAt.UTC(),
		strings.TrimSpace(checkpoint.XrayRevision),
		checkpoint.ResetEpoch,
	).Scan(&checkpoint.CreatedAt, &checkpoint.UpdatedAt)
}

func (s *postgresStore) GetTrafficStatCheckpoint(ctx context.Context, nodeID, accountUUID string) (*TrafficStatCheckpoint, error) {
	const query = `
		SELECT node_id, account_uuid, last_uplink_total, last_downlink_total, last_seen_at, xray_revision, reset_epoch, created_at, updated_at
		FROM traffic_stat_checkpoints
		WHERE node_id = $1 AND account_uuid = $2`
	var record TrafficStatCheckpoint
	err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(nodeID), strings.TrimSpace(accountUUID)).Scan(
		&record.NodeID,
		&record.AccountUUID,
		&record.LastUplinkTotal,
		&record.LastDownlinkTotal,
		&record.LastSeenAt,
		&record.XrayRevision,
		&record.ResetEpoch,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &record, nil
}

func (s *postgresStore) ListTrafficStatCheckpoints(ctx context.Context) ([]TrafficStatCheckpoint, error) {
	const query = `
		SELECT node_id, account_uuid, last_uplink_total, last_downlink_total, last_seen_at, xray_revision, reset_epoch, created_at, updated_at
		FROM traffic_stat_checkpoints
		ORDER BY node_id ASC, account_uuid ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TrafficStatCheckpoint
	for rows.Next() {
		var record TrafficStatCheckpoint
		if err := rows.Scan(
			&record.NodeID,
			&record.AccountUUID,
			&record.LastUplinkTotal,
			&record.LastDownlinkTotal,
			&record.LastSeenAt,
			&record.XrayRevision,
			&record.ResetEpoch,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *postgresStore) UpsertTrafficMinuteBucket(ctx context.Context, bucket *TrafficMinuteBucket) error {
	if bucket == nil {
		return errors.New("bucket is required")
	}

	status := strings.TrimSpace(bucket.RatingStatus)
	if status == "" {
		status = RatingStatusPending
	}

	const query = `
		INSERT INTO traffic_minute_buckets (
			bucket_start, node_id, account_uuid, region, line_code, uplink_bytes, downlink_bytes, total_bytes, multiplier, rating_status, source_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (bucket_start, node_id, account_uuid, region, line_code) DO UPDATE SET
			uplink_bytes = EXCLUDED.uplink_bytes,
			downlink_bytes = EXCLUDED.downlink_bytes,
			total_bytes = EXCLUDED.total_bytes,
			multiplier = EXCLUDED.multiplier,
			rating_status = EXCLUDED.rating_status,
			source_revision = EXCLUDED.source_revision,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		bucket.BucketStart.UTC(),
		strings.TrimSpace(bucket.NodeID),
		strings.TrimSpace(bucket.AccountUUID),
		strings.TrimSpace(bucket.Region),
		strings.TrimSpace(bucket.LineCode),
		bucket.UplinkBytes,
		bucket.DownlinkBytes,
		bucket.TotalBytes,
		bucket.Multiplier,
		status,
		strings.TrimSpace(bucket.SourceRevision),
	).Scan(&bucket.CreatedAt, &bucket.UpdatedAt)
}

func (s *postgresStore) ListTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time) ([]TrafficMinuteBucket, error) {
	query := `
		SELECT bucket_start, node_id, account_uuid, region, line_code, uplink_bytes, downlink_bytes, total_bytes, multiplier, rating_status, source_revision, created_at, updated_at
		FROM traffic_minute_buckets
		WHERE account_uuid = $1`
	args := []any{strings.TrimSpace(accountUUID)}
	if !start.IsZero() {
		query += " AND bucket_start >= $2"
		args = append(args, start.UTC())
	}
	if !end.IsZero() {
		query += " AND bucket_start <= $" + strconv.Itoa(len(args)+1)
		args = append(args, end.UTC())
	}
	query += " ORDER BY bucket_start ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TrafficMinuteBucket
	for rows.Next() {
		var record TrafficMinuteBucket
		if err := rows.Scan(
			&record.BucketStart,
			&record.NodeID,
			&record.AccountUUID,
			&record.Region,
			&record.LineCode,
			&record.UplinkBytes,
			&record.DownlinkBytes,
			&record.TotalBytes,
			&record.Multiplier,
			&record.RatingStatus,
			&record.SourceRevision,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *postgresStore) ListTrafficMinuteBuckets(ctx context.Context) ([]TrafficMinuteBucket, error) {
	const query = `
		SELECT bucket_start, node_id, account_uuid, region, line_code, uplink_bytes, downlink_bytes, total_bytes, multiplier, rating_status, source_revision, created_at, updated_at
		FROM traffic_minute_buckets
		ORDER BY bucket_start ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TrafficMinuteBucket
	for rows.Next() {
		var record TrafficMinuteBucket
		if err := rows.Scan(
			&record.BucketStart,
			&record.NodeID,
			&record.AccountUUID,
			&record.Region,
			&record.LineCode,
			&record.UplinkBytes,
			&record.DownlinkBytes,
			&record.TotalBytes,
			&record.Multiplier,
			&record.RatingStatus,
			&record.SourceRevision,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *postgresStore) InsertBillingLedgerEntry(ctx context.Context, entry *BillingLedgerEntry) error {
	if entry == nil {
		return errors.New("ledger entry is required")
	}
	if strings.TrimSpace(entry.ID) == "" {
		entry.ID = uuid.NewString()
	}

	const query = `
		INSERT INTO billing_ledger (
			id, account_uuid, bucket_start, bucket_end, entry_type, rated_bytes, amount_delta, balance_after, pricing_rule_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		entry.ID,
		strings.TrimSpace(entry.AccountUUID),
		entry.BucketStart.UTC(),
		entry.BucketEnd.UTC(),
		strings.TrimSpace(entry.EntryType),
		entry.RatedBytes,
		entry.AmountDelta,
		entry.BalanceAfter,
		strings.TrimSpace(entry.PricingRuleVersion),
	).Scan(&entry.CreatedAt)
}

func (s *postgresStore) ListBillingLedgerByAccount(ctx context.Context, accountUUID string, limit int) ([]BillingLedgerEntry, error) {
	query := `
		SELECT id, account_uuid, bucket_start, bucket_end, entry_type, rated_bytes, amount_delta, balance_after, pricing_rule_version, created_at
		FROM billing_ledger
		WHERE account_uuid = $1
		ORDER BY created_at DESC`
	args := []any{strings.TrimSpace(accountUUID)}
	if limit > 0 {
		query += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []BillingLedgerEntry
	for rows.Next() {
		var entry BillingLedgerEntry
		if err := rows.Scan(
			&entry.ID,
			&entry.AccountUUID,
			&entry.BucketStart,
			&entry.BucketEnd,
			&entry.EntryType,
			&entry.RatedBytes,
			&entry.AmountDelta,
			&entry.BalanceAfter,
			&entry.PricingRuleVersion,
			&entry.CreatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *postgresStore) UpsertAccountQuotaState(ctx context.Context, state *AccountQuotaState) error {
	if state == nil {
		return errors.New("quota state is required")
	}

	const query = `
		INSERT INTO account_quota_states (
			account_uuid, remaining_included_quota, current_balance, arrears, arrears_since, throttle_state, suspend_state, last_rated_bucket_at, effective_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (account_uuid) DO UPDATE SET
			remaining_included_quota = EXCLUDED.remaining_included_quota,
			current_balance = EXCLUDED.current_balance,
			arrears = EXCLUDED.arrears,
			arrears_since = EXCLUDED.arrears_since,
			throttle_state = EXCLUDED.throttle_state,
			suspend_state = EXCLUDED.suspend_state,
			last_rated_bucket_at = EXCLUDED.last_rated_bucket_at,
			effective_at = EXCLUDED.effective_at,
			updated_at = now()
		RETURNING updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		strings.TrimSpace(state.AccountUUID),
		state.RemainingIncludedQuota,
		state.CurrentBalance,
		state.Arrears,
		state.ArrearsSince,
		strings.TrimSpace(state.ThrottleState),
		strings.TrimSpace(state.SuspendState),
		state.LastRatedBucketAt,
		state.EffectiveAt.UTC(),
	).Scan(&state.UpdatedAt)
}

func (s *postgresStore) GetAccountQuotaState(ctx context.Context, accountUUID string) (*AccountQuotaState, error) {
	const query = `
		SELECT account_uuid, remaining_included_quota, current_balance, arrears, arrears_since, throttle_state, suspend_state, last_rated_bucket_at, effective_at, updated_at
		FROM account_quota_states
		WHERE account_uuid = $1`
	var state AccountQuotaState
	err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(accountUUID)).Scan(
		&state.AccountUUID,
		&state.RemainingIncludedQuota,
		&state.CurrentBalance,
		&state.Arrears,
		&state.ArrearsSince,
		&state.ThrottleState,
		&state.SuspendState,
		&state.LastRatedBucketAt,
		&state.EffectiveAt,
		&state.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &state, nil
}

func (s *postgresStore) MarkAccountArrears(ctx context.Context, accountUUID string, occurredAt time.Time) error {
	const query = `INSERT INTO account_quota_states (account_uuid, arrears, arrears_since, throttle_state, suspend_state, effective_at)
		VALUES ($1, true, $2, 'normal', 'active', $2)
		ON CONFLICT (account_uuid) DO UPDATE SET arrears = true, arrears_since = COALESCE(account_quota_states.arrears_since, EXCLUDED.arrears_since), updated_at = now()`
	_, err := s.db.ExecContext(ctx, query, strings.TrimSpace(accountUUID), occurredAt.UTC())
	return err
}

func (s *postgresStore) ClearAccountArrears(ctx context.Context, accountUUID string) error {
	const query = `UPDATE account_quota_states SET arrears = false, arrears_since = NULL, throttle_state = 'normal', suspend_state = 'active', updated_at = now() WHERE account_uuid = $1`
	_, err := s.db.ExecContext(ctx, query, strings.TrimSpace(accountUUID))
	return err
}

func (s *postgresStore) IsAccountSuspended(ctx context.Context, accountUUID string, now time.Time) (bool, error) {
	const promote = `UPDATE account_quota_states SET suspend_state = 'suspended', updated_at = now()
		WHERE account_uuid = $1 AND arrears = true AND arrears_since <= $2 AND suspend_state <> 'suspended'`
	if _, err := s.db.ExecContext(ctx, promote, strings.TrimSpace(accountUUID), now.UTC().Add(-14*24*time.Hour)); err != nil {
		return false, err
	}
	var suspended bool
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(suspend_state = 'suspended', false) FROM account_quota_states WHERE account_uuid = $1`, strings.TrimSpace(accountUUID)).Scan(&suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return suspended, err
}

func (s *postgresStore) UpsertAccountBillingProfile(ctx context.Context, profile *AccountBillingProfile) error {
	if profile == nil {
		return errors.New("billing profile is required")
	}

	const query = `
		INSERT INTO account_billing_profiles (
			account_uuid, package_name, included_quota_bytes, base_price_per_byte, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (account_uuid) DO UPDATE SET
			package_name = EXCLUDED.package_name,
			included_quota_bytes = EXCLUDED.included_quota_bytes,
			base_price_per_byte = EXCLUDED.base_price_per_byte,
			region_multiplier = EXCLUDED.region_multiplier,
			line_multiplier = EXCLUDED.line_multiplier,
			peak_multiplier = EXCLUDED.peak_multiplier,
			offpeak_multiplier = EXCLUDED.offpeak_multiplier,
			pricing_rule_version = EXCLUDED.pricing_rule_version,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		strings.TrimSpace(profile.AccountUUID),
		strings.TrimSpace(profile.PackageName),
		profile.IncludedQuotaBytes,
		profile.BasePricePerByte,
		profile.RegionMultiplier,
		profile.LineMultiplier,
		profile.PeakMultiplier,
		profile.OffPeakMultiplier,
		strings.TrimSpace(profile.PricingRuleVersion),
	).Scan(&profile.CreatedAt, &profile.UpdatedAt)
}

func (s *postgresStore) GetAccountBillingProfile(ctx context.Context, accountUUID string) (*AccountBillingProfile, error) {
	const query = `
		SELECT account_uuid, package_name, included_quota_bytes, base_price_per_byte, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version, created_at, updated_at
		FROM account_billing_profiles
		WHERE account_uuid = $1`
	var profile AccountBillingProfile
	err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(accountUUID)).Scan(
		&profile.AccountUUID,
		&profile.PackageName,
		&profile.IncludedQuotaBytes,
		&profile.BasePricePerByte,
		&profile.RegionMultiplier,
		&profile.LineMultiplier,
		&profile.PeakMultiplier,
		&profile.OffPeakMultiplier,
		&profile.PricingRuleVersion,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &profile, nil
}

func (s *postgresStore) UpsertAccountPolicySnapshot(ctx context.Context, snapshot *AccountPolicySnapshot) error {
	if snapshot == nil {
		return errors.New("policy snapshot is required")
	}

	groups, err := encodeStringSlice(snapshot.EligibleNodeGroups)
	if err != nil {
		return err
	}

	const query = `
		INSERT INTO account_policy_snapshots (
			account_uuid, policy_version, auth_state, rate_profile, conn_profile, eligible_node_groups, preferred_strategy, degrade_mode, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (account_uuid) DO UPDATE SET
			policy_version = EXCLUDED.policy_version,
			auth_state = EXCLUDED.auth_state,
			rate_profile = EXCLUDED.rate_profile,
			conn_profile = EXCLUDED.conn_profile,
			eligible_node_groups = EXCLUDED.eligible_node_groups,
			preferred_strategy = EXCLUDED.preferred_strategy,
			degrade_mode = EXCLUDED.degrade_mode,
			expires_at = EXCLUDED.expires_at,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		strings.TrimSpace(snapshot.AccountUUID),
		strings.TrimSpace(snapshot.PolicyVersion),
		strings.TrimSpace(snapshot.AuthState),
		strings.TrimSpace(snapshot.RateProfile),
		strings.TrimSpace(snapshot.ConnProfile),
		groups,
		strings.TrimSpace(snapshot.PreferredStrategy),
		strings.TrimSpace(snapshot.DegradeMode),
		snapshot.ExpiresAt.UTC(),
	).Scan(&snapshot.CreatedAt, &snapshot.UpdatedAt)
}

func (s *postgresStore) GetLatestAccountPolicySnapshot(ctx context.Context, accountUUID string) (*AccountPolicySnapshot, error) {
	const query = `
		SELECT account_uuid, policy_version, auth_state, rate_profile, conn_profile, eligible_node_groups, preferred_strategy, degrade_mode, expires_at, created_at, updated_at
		FROM account_policy_snapshots
		WHERE account_uuid = $1`
	var snapshot AccountPolicySnapshot
	var groups []byte
	err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(accountUUID)).Scan(
		&snapshot.AccountUUID,
		&snapshot.PolicyVersion,
		&snapshot.AuthState,
		&snapshot.RateProfile,
		&snapshot.ConnProfile,
		&groups,
		&snapshot.PreferredStrategy,
		&snapshot.DegradeMode,
		&snapshot.ExpiresAt,
		&snapshot.CreatedAt,
		&snapshot.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	snapshot.EligibleNodeGroups = decodeStringSlice(groups)
	return &snapshot, nil
}

func (s *postgresStore) UpsertNodeHealthSnapshot(ctx context.Context, snapshot *NodeHealthSnapshot) error {
	if snapshot == nil {
		return errors.New("node health snapshot is required")
	}

	const query = `
		INSERT INTO node_health_snapshots (
			node_id, region, line_code, pricing_group, stats_enabled, xray_revision, healthy, latency_ms, error_rate, active_connections, health_score, sampled_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (node_id) DO UPDATE SET
			region = EXCLUDED.region,
			line_code = EXCLUDED.line_code,
			pricing_group = EXCLUDED.pricing_group,
			stats_enabled = EXCLUDED.stats_enabled,
			xray_revision = EXCLUDED.xray_revision,
			healthy = EXCLUDED.healthy,
			latency_ms = EXCLUDED.latency_ms,
			error_rate = EXCLUDED.error_rate,
			active_connections = EXCLUDED.active_connections,
			health_score = EXCLUDED.health_score,
			sampled_at = EXCLUDED.sampled_at,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		strings.TrimSpace(snapshot.NodeID),
		strings.TrimSpace(snapshot.Region),
		strings.TrimSpace(snapshot.LineCode),
		strings.TrimSpace(snapshot.PricingGroup),
		snapshot.StatsEnabled,
		strings.TrimSpace(snapshot.XrayRevision),
		snapshot.Healthy,
		snapshot.LatencyMS,
		snapshot.ErrorRate,
		snapshot.ActiveConnections,
		snapshot.HealthScore,
		snapshot.SampledAt.UTC(),
	).Scan(&snapshot.CreatedAt, &snapshot.UpdatedAt)
}

func (s *postgresStore) ListLatestNodeHealthSnapshots(ctx context.Context) ([]NodeHealthSnapshot, error) {
	const query = `
		SELECT node_id, region, line_code, pricing_group, stats_enabled, xray_revision, healthy, latency_ms, error_rate, active_connections, health_score, sampled_at, created_at, updated_at
		FROM node_health_snapshots
		ORDER BY node_id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []NodeHealthSnapshot
	for rows.Next() {
		var snapshot NodeHealthSnapshot
		if err := rows.Scan(
			&snapshot.NodeID,
			&snapshot.Region,
			&snapshot.LineCode,
			&snapshot.PricingGroup,
			&snapshot.StatsEnabled,
			&snapshot.XrayRevision,
			&snapshot.Healthy,
			&snapshot.LatencyMS,
			&snapshot.ErrorRate,
			&snapshot.ActiveConnections,
			&snapshot.HealthScore,
			&snapshot.SampledAt,
			&snapshot.CreatedAt,
			&snapshot.UpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	return result, rows.Err()
}

func (s *postgresStore) InsertSchedulerDecision(ctx context.Context, decision *SchedulerDecision) error {
	if decision == nil {
		return errors.New("scheduler decision is required")
	}
	if strings.TrimSpace(decision.ID) == "" {
		decision.ID = uuid.NewString()
	}

	const query = `
		INSERT INTO scheduler_decisions (id, account_uuid, node_group, strategy, decision, generated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		decision.ID,
		strings.TrimSpace(decision.AccountUUID),
		strings.TrimSpace(decision.NodeGroup),
		strings.TrimSpace(decision.Strategy),
		strings.TrimSpace(decision.Decision),
		decision.GeneratedAt.UTC(),
	).Scan(&decision.CreatedAt)
}

func (s *postgresStore) ListRecentSchedulerDecisions(ctx context.Context, limit int) ([]SchedulerDecision, error) {
	query := `
		SELECT id, account_uuid, node_group, strategy, decision, generated_at, created_at
		FROM scheduler_decisions
		ORDER BY generated_at DESC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT $1"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SchedulerDecision
	for rows.Next() {
		var decision SchedulerDecision
		if err := rows.Scan(
			&decision.ID,
			&decision.AccountUUID,
			&decision.NodeGroup,
			&decision.Strategy,
			&decision.Decision,
			&decision.GeneratedAt,
			&decision.CreatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, decision)
	}
	return result, rows.Err()
}
