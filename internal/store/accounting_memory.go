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
	}
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = now
	}
	s.billingLedgerEntries[copy.ID] = copy
	entry.ID = copy.ID
	entry.CreatedAt = copy.CreatedAt
	return nil
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
	copy.UpdatedAt = time.Now().UTC()
	if copy.EffectiveAt.IsZero() {
		copy.EffectiveAt = copy.UpdatedAt
	}
	s.accountQuotaStates[strings.TrimSpace(copy.AccountUUID)] = copy
	return nil
}

func (s *memoryStore) GetAccountQuotaState(ctx context.Context, accountUUID string) (*AccountQuotaState, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.accountQuotaStates[strings.TrimSpace(accountUUID)]
	if !ok {
		return nil, ErrUserNotFound
	}
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
