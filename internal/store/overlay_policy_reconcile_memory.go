package store

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (s *memoryStore) MarkOverlayPolicyReconcilePending(_ context.Context, networkID, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.overlayPolicyPending[strings.TrimSpace(networkID)]
	item.NetworkID = strings.TrimSpace(networkID)
	item.Attempts++
	item.LastError = strings.TrimSpace(lastError)
	item.UpdatedAt = time.Now().UTC()
	s.overlayPolicyPending[item.NetworkID] = item
	return nil
}
func (s *memoryStore) ClearOverlayPolicyReconcilePending(_ context.Context, networkID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.overlayPolicyPending, strings.TrimSpace(networkID))
	return nil
}
func (s *memoryStore) ListOverlayPolicyReconcilePending(_ context.Context, limit int) ([]OverlayPolicyReconcilePending, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]OverlayPolicyReconcilePending, 0, len(s.overlayPolicyPending))
	for _, item := range s.overlayPolicyPending {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
