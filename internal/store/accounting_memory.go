package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func checkpointKey(nodeID, accountUUID string) string {
	return strings.TrimSpace(nodeID) + "::" + strings.TrimSpace(accountUUID)
}

func bucketKey(bucketStart time.Time, nodeID, accountUUID, region, lineCode string) string {
	return fmt.Sprintf("%s::%s::%s::%s::%s",
		bucketStart.UTC().Format(time.RFC3339),
		strings.TrimSpace(nodeID),
		strings.TrimSpace(accountUUID),
		strings.TrimSpace(region),
		strings.TrimSpace(lineCode),
	)
}

func cloneCheckpoint(src *TrafficStatCheckpoint) *TrafficStatCheckpoint {
	if src == nil {
		return nil
	}
	copy := *src
	return &copy
}

func cloneBucket(src *TrafficMinuteBucket) *TrafficMinuteBucket {
	if src == nil {
		return nil
	}
	copy := *src
	return &copy
}

func cloneLedgerEntry(src *BillingLedgerEntry) *BillingLedgerEntry {
	if src == nil {
		return nil
	}
	copy := *src
	return &copy
}

func cloneQuotaState(src *AccountQuotaState) *AccountQuotaState {
	if src == nil {
		return nil
	}
	copy := *src
	if src.LastRatedBucketAt != nil {
		last := src.LastRatedBucketAt.UTC()
		copy.LastRatedBucketAt = &last
	}
	if src.ArrearsSince != nil {
		since := src.ArrearsSince.UTC()
		copy.ArrearsSince = &since
	}
	if src.PeriodStart != nil {
		start := src.PeriodStart.UTC()
		copy.PeriodStart = &start
	}
	if src.PeriodEnd != nil {
		end := src.PeriodEnd.UTC()
		copy.PeriodEnd = &end
	}
	return &copy
}

func cloneBillingProfile(src *AccountBillingProfile) *AccountBillingProfile {
	if src == nil {
		return nil
	}
	copy := *src
	return &copy
}

func clonePolicySnapshot(src *AccountPolicySnapshot) *AccountPolicySnapshot {
	if src == nil {
		return nil
	}
	copy := *src
	copy.EligibleNodeGroups = cloneStringSlice(src.EligibleNodeGroups)
	return &copy
}

func cloneNodeHealthSnapshot(src *NodeHealthSnapshot) *NodeHealthSnapshot {
	if src == nil {
		return nil
	}
	copy := *src
	return &copy
}

func cloneSchedulerDecision(src *SchedulerDecision) *SchedulerDecision {
	if src == nil {
		return nil
	}
	copy := *src
	return &copy
}

func (s *memoryStore) UpsertTrafficStatCheckpoint(ctx context.Context, checkpoint *TrafficStatCheckpoint) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneCheckpoint(checkpoint)
	if copy == nil {
		return errors.New("checkpoint is required")
	}
	now := time.Now().UTC()
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	copy.UpdatedAt = now
	s.trafficStatCheckpoints[checkpointKey(copy.NodeID, copy.AccountUUID)] = copy
	return nil
}

func (s *memoryStore) GetTrafficStatCheckpoint(ctx context.Context, nodeID, accountUUID string) (*TrafficStatCheckpoint, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.trafficStatCheckpoints[checkpointKey(nodeID, accountUUID)]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneCheckpoint(record), nil
}

func (s *memoryStore) ListTrafficStatCheckpoints(ctx context.Context) ([]TrafficStatCheckpoint, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]TrafficStatCheckpoint, 0, len(s.trafficStatCheckpoints))
	for _, record := range s.trafficStatCheckpoints {
		result = append(result, *cloneCheckpoint(record))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NodeID == result[j].NodeID {
			return result[i].AccountUUID < result[j].AccountUUID
		}
		return result[i].NodeID < result[j].NodeID
	})
	return result, nil
}

func (s *memoryStore) UpsertTrafficMinuteBucket(ctx context.Context, bucket *TrafficMinuteBucket) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneBucket(bucket)
	if copy == nil {
		return errors.New("bucket is required")
	}
	now := time.Now().UTC()
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	copy.UpdatedAt = now
	copy.RoutingPool = strings.TrimSpace(copy.RoutingPool)
	if strings.TrimSpace(copy.RatingStatus) == "" {
		copy.RatingStatus = RatingStatusPending
	}
	s.trafficMinuteBuckets[bucketKey(copy.BucketStart, copy.NodeID, copy.AccountUUID, copy.Region, copy.LineCode)] = copy
	return nil
}

func (s *memoryStore) ListTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time) ([]TrafficMinuteBucket, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]TrafficMinuteBucket, 0)
	for _, bucket := range s.trafficMinuteBuckets {
		if bucket.AccountUUID != strings.TrimSpace(accountUUID) {
			continue
		}
		if !start.IsZero() && bucket.BucketStart.Before(start) {
			continue
		}
		if !end.IsZero() && bucket.BucketStart.After(end) {
			continue
		}
		result = append(result, *cloneBucket(bucket))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].BucketStart.Before(result[j].BucketStart)
	})
	return result, nil
}

func (s *memoryStore) SummarizeTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time) ([]AccountTrafficNodeSummary, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	type summaryKey struct {
		nodeID      string
		routingPool string
	}
	targetAccount := strings.TrimSpace(accountUUID)
	grouped := make(map[summaryKey]*AccountTrafficNodeSummary)
	for _, bucket := range s.trafficMinuteBuckets {
		if bucket.AccountUUID != targetAccount {
			continue
		}
		if !start.IsZero() && bucket.BucketStart.Before(start) {
			continue
		}
		if !end.IsZero() && bucket.BucketStart.After(end) {
			continue
		}
		key := summaryKey{
			nodeID:      strings.TrimSpace(bucket.NodeID),
			routingPool: strings.TrimSpace(bucket.RoutingPool),
		}
		entry := grouped[key]
		if entry == nil {
			entry = &AccountTrafficNodeSummary{
				NodeID:      key.nodeID,
				RoutingPool: key.routingPool,
			}
			grouped[key] = entry
		}
		entry.UplinkBytes += bucket.UplinkBytes
		entry.DownlinkBytes += bucket.DownlinkBytes
		entry.TotalBytes += bucket.TotalBytes
		ts := bucket.BucketStart.UTC()
		if entry.LastBucketAt == nil || ts.After(*entry.LastBucketAt) {
			entry.LastBucketAt = &ts
		}
	}

	result := make([]AccountTrafficNodeSummary, 0, len(grouped))
	for _, entry := range grouped {
		result = append(result, *entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NodeID != result[j].NodeID {
			return result[i].NodeID < result[j].NodeID
		}
		return result[i].RoutingPool < result[j].RoutingPool
	})
	return result, nil
}

func (s *memoryStore) CountTrafficMinuteBuckets(ctx context.Context) (int64, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.trafficMinuteBuckets)), nil
}

func (s *memoryStore) ListCompactTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time, minutePrecisionAfter time.Time) ([]TrafficMinuteBucket, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	targetAccount := strings.TrimSpace(accountUUID)
	hasCutoff := !minutePrecisionAfter.IsZero()
	cutoff := minutePrecisionAfter.UTC().Truncate(time.Hour)

	type rollupKey struct {
		hourStart   time.Time
		nodeID      string
		accountUUID string
		region      string
		lineCode    string
		routingPool string
	}
	rolledUp := make(map[rollupKey]*TrafficMinuteBucket)
	result := make([]TrafficMinuteBucket, 0)

	for _, bucket := range s.trafficMinuteBuckets {
		if bucket.AccountUUID != targetAccount {
			continue
		}
		if bucket.TotalBytes == 0 && bucket.UplinkBytes == 0 && bucket.DownlinkBytes == 0 {
			continue
		}
		if !start.IsZero() && bucket.BucketStart.Before(start) {
			continue
		}
		if !end.IsZero() && bucket.BucketStart.After(end) {
			continue
		}
		if hasCutoff && bucket.BucketStart.Before(cutoff) {
			ts := bucket.BucketStart.UTC()
			key := rollupKey{
				hourStart:   ts.Truncate(time.Hour),
				nodeID:      strings.TrimSpace(bucket.NodeID),
				accountUUID: bucket.AccountUUID,
				region:      strings.TrimSpace(bucket.Region),
				lineCode:    strings.TrimSpace(bucket.LineCode),
				routingPool: strings.TrimSpace(bucket.RoutingPool),
			}
			agg := rolledUp[key]
			if agg == nil {
				agg = &TrafficMinuteBucket{
					BucketStart:   ts,
					NodeID:        key.nodeID,
					AccountUUID:   key.accountUUID,
					Region:        key.region,
					LineCode:      key.lineCode,
					RoutingPool:   key.routingPool,
					UplinkBytes:   bucket.UplinkBytes,
					DownlinkBytes: bucket.DownlinkBytes,
					TotalBytes:    bucket.TotalBytes,
					Multiplier:    bucket.Multiplier,
					RatingStatus:  bucket.RatingStatus,
					CreatedAt:     bucket.CreatedAt,
					UpdatedAt:     bucket.UpdatedAt,
				}
				rolledUp[key] = agg
				continue
			}
			if ts.Before(agg.BucketStart) {
				agg.BucketStart = ts
			}
			agg.UplinkBytes += bucket.UplinkBytes
			agg.DownlinkBytes += bucket.DownlinkBytes
			agg.TotalBytes += bucket.TotalBytes
			if bucket.Multiplier > agg.Multiplier {
				agg.Multiplier = bucket.Multiplier
			}
			if bucket.RatingStatus > agg.RatingStatus {
				agg.RatingStatus = bucket.RatingStatus
			}
			if !bucket.CreatedAt.IsZero() && (agg.CreatedAt.IsZero() || bucket.CreatedAt.Before(agg.CreatedAt)) {
				agg.CreatedAt = bucket.CreatedAt
			}
			if bucket.UpdatedAt.After(agg.UpdatedAt) {
				agg.UpdatedAt = bucket.UpdatedAt
			}
			continue
		}
		result = append(result, *cloneBucket(bucket))
	}

	for _, agg := range rolledUp {
		result = append(result, *agg)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].BucketStart.Equal(result[j].BucketStart) {
			return result[i].BucketStart.Before(result[j].BucketStart)
		}
		return result[i].NodeID < result[j].NodeID
	})
	return result, nil
}

func (s *memoryStore) ListTrafficMinuteBuckets(ctx context.Context) ([]TrafficMinuteBucket, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]TrafficMinuteBucket, 0, len(s.trafficMinuteBuckets))
	for _, bucket := range s.trafficMinuteBuckets {
		result = append(result, *cloneBucket(bucket))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].BucketStart.Before(result[j].BucketStart)
	})
	return result, nil
}

func (s *memoryStore) InsertBillingLedgerEntry(ctx context.Context, entry *BillingLedgerEntry) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneLedgerEntry(entry)
	if copy == nil {
		return errors.New("ledger entry is required")
	}
	now := time.Now().UTC()
	if strings.TrimSpace(copy.ID) == "" {
		copy.ID = uuid.NewString()
	} else if _, exists := s.billingLedgerEntries[copy.ID]; exists {
		// billing_ledger.id is a primary key in postgres, so a caller-supplied
		// duplicate is rejected there. Mirror that here: callers use the id as
		// an idempotency lock (see creditTopUpBalance), and a memory store that
		// silently overwrote would let a double-credit bug pass its tests.
		return ErrDuplicateLedgerEntry
	}
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	s.billingLedgerEntries[copy.ID] = copy
	entry.ID = copy.ID
	entry.CreatedAt = copy.CreatedAt
	return nil
}

func (s *memoryStore) CreditTopUp(ctx context.Context, entry *BillingLedgerEntry) (bool, error) {
	_ = ctx
	if entry == nil || strings.TrimSpace(entry.ID) == "" || entry.AmountDelta <= 0 {
		return false, errors.New("valid top-up ledger entry is required")
	}
	currency := strings.ToUpper(strings.TrimSpace(entry.Currency))
	if currency != "CNY" && currency != "USD" {
		return false, errors.New("unsupported wallet currency")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.billingLedgerEntries[entry.ID]; exists {
		return true, nil
	}
	accountID := strings.TrimSpace(entry.AccountUUID)
	state := cloneQuotaState(s.accountQuotaStates[accountID])
	if state == nil {
		state = &AccountQuotaState{AccountUUID: accountID}
	}
	if currency == "CNY" {
		entry.BalanceAfter = state.CurrentBalance + entry.AmountDelta
		state.CurrentBalance = entry.BalanceAfter
		state.EffectiveAt = time.Now().UTC()
		state.UpdatedAt = state.EffectiveAt
		if state.CurrentBalance >= 0 {
			state.Arrears = false
			state.ArrearsSince = nil
		}
	} else {
		for _, prior := range s.billingLedgerEntries {
			if prior.AccountUUID == accountID && prior.EntryType == "wallet_usd" {
				entry.BalanceAfter += prior.AmountDelta
			}
		}
		entry.BalanceAfter += entry.AmountDelta
	}
	entry.Currency = currency
	if currency == "USD" {
		entry.EntryType = "wallet_usd"
	} else {
		entry.EntryType = "topup"
	}
	now := time.Now().UTC()
	entry.CreatedAt = now
	s.billingLedgerEntries[entry.ID] = cloneLedgerEntry(entry)
	s.accountQuotaStates[accountID] = state
	return false, nil
}

func (s *memoryStore) GetWalletBalance(ctx context.Context, accountUUID, currency string) (float64, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "CNY":
		if quota := s.accountQuotaStates[strings.TrimSpace(accountUUID)]; quota != nil {
			return quota.CurrentBalance, nil
		}
		return 0, nil
	case "USD":
		var balance float64
		for _, entry := range s.billingLedgerEntries {
			if entry.AccountUUID == strings.TrimSpace(accountUUID) && entry.EntryType == "wallet_usd" {
				balance += entry.AmountDelta
			}
		}
		return balance, nil
	default:
		return 0, errors.New("unsupported wallet currency")
	}
}

func (s *memoryStore) ListBillingLedgerByAccount(ctx context.Context, accountUUID string, limit int) ([]BillingLedgerEntry, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]BillingLedgerEntry, 0)
	for _, entry := range s.billingLedgerEntries {
		if entry.AccountUUID == strings.TrimSpace(accountUUID) {
			result = append(result, *cloneLedgerEntry(entry))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) UpsertAccountQuotaState(ctx context.Context, state *AccountQuotaState) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneQuotaState(state)
	if copy == nil {
		return errors.New("quota state is required")
	}
	if strings.TrimSpace(copy.ProxyAccessState) == "" {
		copy.ProxyAccessState = "active"
	}
	if strings.TrimSpace(copy.ThrottleState) == "" {
		copy.ThrottleState = "normal"
	}
	if strings.TrimSpace(copy.SuspendState) == "" {
		copy.SuspendState = "active"
	}
	copy.UpdatedAt = time.Now().UTC()
	if copy.EffectiveAt.IsZero() {
		copy.EffectiveAt = copy.UpdatedAt
	}
	s.accountQuotaStates[strings.TrimSpace(copy.AccountUUID)] = copy
	return nil
}

func (s *memoryStore) GetAccountQuotaState(ctx context.Context, accountUUID string) (*AccountQuotaState, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.accountQuotaStates[strings.TrimSpace(accountUUID)]
	if !ok {
		return nil, ErrUserNotFound
	}
	s.expireMonthlyUsageResetLocked(accountUUID, time.Now().UTC())
	return cloneQuotaState(record), nil
}

func (s *memoryStore) ListSuspendedAccountUUIDs(ctx context.Context) (map[string]bool, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	suspended := make(map[string]bool)
	for accountUUID, record := range s.accountQuotaStates {
		if record != nil && record.SuspendState == "suspended" {
			suspended[accountUUID] = true
		}
	}
	return suspended, nil
}

func (s *memoryStore) ListProxyBlockedAccountUUIDs(ctx context.Context) (map[string]bool, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	blocked := make(map[string]bool)
	for accountUUID, record := range s.accountQuotaStates {
		if record != nil && (record.SuspendState == "suspended" || record.ProxyAccessState == "paused") {
			blocked[accountUUID] = true
		}
	}
	return blocked, nil
}

func (s *memoryStore) UpsertAccountBillingProfile(ctx context.Context, profile *AccountBillingProfile) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneBillingProfile(profile)
	if copy == nil {
		return errors.New("billing profile is required")
	}
	now := time.Now().UTC()
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	copy.UpdatedAt = now
	s.accountBillingProfiles[strings.TrimSpace(copy.AccountUUID)] = copy
	return nil
}

func (s *memoryStore) GetAccountBillingProfile(ctx context.Context, accountUUID string) (*AccountBillingProfile, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.accountBillingProfiles[strings.TrimSpace(accountUUID)]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneBillingProfile(record), nil
}

func (s *memoryStore) UpsertAccountPolicySnapshot(ctx context.Context, snapshot *AccountPolicySnapshot) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := clonePolicySnapshot(snapshot)
	if copy == nil {
		return errors.New("policy snapshot is required")
	}
	now := time.Now().UTC()
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	copy.UpdatedAt = now
	s.accountPolicySnapshots[strings.TrimSpace(copy.AccountUUID)] = copy
	return nil
}

func (s *memoryStore) GetLatestAccountPolicySnapshot(ctx context.Context, accountUUID string) (*AccountPolicySnapshot, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.accountPolicySnapshots[strings.TrimSpace(accountUUID)]
	if !ok {
		return nil, ErrUserNotFound
	}
	return clonePolicySnapshot(record), nil
}

func (s *memoryStore) UpsertNodeHealthSnapshot(ctx context.Context, snapshot *NodeHealthSnapshot) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneNodeHealthSnapshot(snapshot)
	if copy == nil {
		return errors.New("node health snapshot is required")
	}
	now := time.Now().UTC()
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	copy.UpdatedAt = now
	if copy.SampledAt.IsZero() {
		copy.SampledAt = now
	}
	s.nodeHealthSnapshots[strings.TrimSpace(copy.NodeID)] = copy
	return nil
}

func (s *memoryStore) ListLatestNodeHealthSnapshots(ctx context.Context) ([]NodeHealthSnapshot, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]NodeHealthSnapshot, 0, len(s.nodeHealthSnapshots))
	for _, snapshot := range s.nodeHealthSnapshots {
		result = append(result, *cloneNodeHealthSnapshot(snapshot))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].NodeID < result[j].NodeID
	})
	return result, nil
}

func (s *memoryStore) InsertSchedulerDecision(ctx context.Context, decision *SchedulerDecision) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	copy := cloneSchedulerDecision(decision)
	if copy == nil {
		return errors.New("scheduler decision is required")
	}
	now := time.Now().UTC()
	if strings.TrimSpace(copy.ID) == "" {
		copy.ID = uuid.NewString()
	}
	if copy.GeneratedAt.IsZero() {
		copy.GeneratedAt = now
	}
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	s.schedulerDecisions[copy.ID] = copy
	decision.ID = copy.ID
	decision.GeneratedAt = copy.GeneratedAt
	decision.CreatedAt = copy.CreatedAt
	return nil
}

func (s *memoryStore) ListRecentSchedulerDecisions(ctx context.Context, limit int) ([]SchedulerDecision, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]SchedulerDecision, 0, len(s.schedulerDecisions))
	for _, decision := range s.schedulerDecisions {
		result = append(result, *cloneSchedulerDecision(decision))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].GeneratedAt.After(result[j].GeneratedAt)
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
