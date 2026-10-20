package store

import (
	"errors"
	"time"
)

const SubscriptionChangeCooldown = 30 * 24 * time.Hour

var (
	ErrSubscriptionCooldown      = errors.New("subscription change cooldown is active")
	ErrSubscriptionStateConflict = errors.New("subscription state version or operation conflicts")
	ErrSubscriptionChangePending = errors.New("subscription has a pending change")
)

// SubscriptionLifecycle belongs to the user, not a provider subscription. It
// must survive cancellation, repurchase and provider-ID changes.
type SubscriptionLifecycle struct {
	AccountID           string     `json:"accountId"`
	Version             int64      `json:"version"`
	LastChangeAt        *time.Time `json:"lastChangeAt,omitempty"`
	OperationID         string     `json:"operationId,omitempty"`
	PendingPlanID       string     `json:"pendingPlanId,omitempty"`
	EffectiveAt         *time.Time `json:"effectiveAt,omitempty"`
	RefundReviewPending bool       `json:"refundReviewPending"`
}

func (s SubscriptionLifecycle) CooldownUntil() *time.Time {
	if s.LastChangeAt == nil {
		return nil
	}
	t := s.LastChangeAt.Add(SubscriptionChangeCooldown)
	return &t
}

// CheckSubscriptionAction is shared by reservation and preview. A network
// timeout is NOT a failed action: its operation must remain reserved until the
// provider outcome is proved. Pending review itself does not reserve this gate.
func CheckSubscriptionAction(s SubscriptionLifecycle, action string, now time.Time) error {
	if s.OperationID != "" {
		return ErrFinanceOperationInProgress
	}
	switch action {
	case "subscribe", "upgrade", "downgrade":
		if s.RefundReviewPending && action != "subscribe" {
			return ErrSubscriptionChangePending
		}
		if s.PendingPlanID != "" {
			return ErrSubscriptionChangePending
		}
	case "cancel", "refund_execute":
	default:
		return ErrSubscriptionStateConflict
	}
	if action == "upgrade" || action == "downgrade" {
		if until := s.CooldownUntil(); until != nil && now.Before(*until) {
			return ErrSubscriptionCooldown
		}
	}
	return nil
}
