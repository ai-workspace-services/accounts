package store

import (
	"context"
	"sort"
	"strings"
	"time"
)

func normalizePlanID(planID string) string {
	return strings.ToUpper(strings.TrimSpace(planID))
}

func (s *memoryStore) ListBillingPlans(ctx context.Context, includeInactive bool) ([]BillingPlan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	plans := make([]BillingPlan, 0, len(s.billingPlans))
	for _, plan := range s.billingPlans {
		if !includeInactive && !plan.Active {
			continue
		}
		plans = append(plans, *plan)
	}
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].SortOrder != plans[j].SortOrder {
			return plans[i].SortOrder < plans[j].SortOrder
		}
		return plans[i].PlanID < plans[j].PlanID
	})
	return plans, nil
}

func (s *memoryStore) GetBillingPlan(ctx context.Context, planID string) (*BillingPlan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan, ok := s.billingPlans[normalizePlanID(planID)]
	if !ok {
		return nil, ErrBillingPlanNotFound
	}
	copied := *plan
	return &copied, nil
}

func (s *memoryStore) GetBillingPlanByPriceID(ctx context.Context, stripePriceID string) (*BillingPlan, error) {
	trimmed := strings.TrimSpace(stripePriceID)
	if trimmed == "" {
		return nil, ErrBillingPlanNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, plan := range s.billingPlans {
		if plan.StripePriceID == trimmed {
			copied := *plan
			return &copied, nil
		}
	}
	return nil, ErrBillingPlanNotFound
}

func (s *memoryStore) UpsertBillingPlan(ctx context.Context, plan *BillingPlan) error {
	if plan == nil || strings.TrimSpace(plan.PlanID) == "" {
		return ErrBillingPlanNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	stored := *plan
	stored.PlanID = normalizePlanID(plan.PlanID)
	stored.UpdatedAt = now
	if existing, ok := s.billingPlans[stored.PlanID]; ok {
		stored.CreatedAt = existing.CreatedAt
	} else {
		stored.CreatedAt = now
	}
	s.billingPlans[stored.PlanID] = &stored
	return nil
}

func (s *memoryStore) DeleteBillingPlan(ctx context.Context, planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := normalizePlanID(planID)
	if _, ok := s.billingPlans[key]; !ok {
		return ErrBillingPlanNotFound
	}
	delete(s.billingPlans, key)
	return nil
}

func (s *memoryStore) BeginStripeWebhookEvent(ctx context.Context, event *StripeWebhookEvent) (bool, error) {
	if event == nil || strings.TrimSpace(event.EventID) == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.stripeWebhookEvents[event.EventID]; ok {
		return existing.Status == StripeWebhookEventStatusProcessed, nil
	}
	stored := *event
	stored.Status = StripeWebhookEventStatusReceived
	stored.ReceivedAt = time.Now().UTC()
	s.stripeWebhookEvents[event.EventID] = &stored
	return false, nil
}

func (s *memoryStore) FinishStripeWebhookEvent(ctx context.Context, eventID string, procErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.stripeWebhookEvents[strings.TrimSpace(eventID)]
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	event.ProcessedAt = &now
	if procErr != nil {
		event.Status = StripeWebhookEventStatusFailed
		event.LastError = procErr.Error()
		return nil
	}
	event.Status = StripeWebhookEventStatusProcessed
	event.LastError = ""
	return nil
}
