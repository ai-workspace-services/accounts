package store

import (
	"errors"
	"testing"
	"time"
)

func TestSubscriptionLifecycleRollingBoundary(t *testing.T) {
	last := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	s := SubscriptionLifecycle{LastChangeAt: &last}
	for _, action := range []string{"upgrade", "downgrade"} {
		if !errors.Is(CheckSubscriptionAction(s, action, last.Add(SubscriptionChangeCooldown-time.Nanosecond)), ErrSubscriptionCooldown) {
			t.Fatal(action, "released early")
		}
		if err := CheckSubscriptionAction(s, action, last.Add(SubscriptionChangeCooldown)); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"subscribe", "cancel", "refund_execute"} {
		if err := CheckSubscriptionAction(s, action, last); err != nil {
			t.Fatal(action, err)
		}
	}
}

func TestSubscriptionLifecyclePendingAndUnknown(t *testing.T) {
	s := SubscriptionLifecycle{PendingPlanID: "PLUS"}
	for _, action := range []string{"subscribe", "upgrade", "downgrade"} {
		if !errors.Is(CheckSubscriptionAction(s, action, time.Now()), ErrSubscriptionChangePending) {
			t.Fatal(action)
		}
	}
	if err := CheckSubscriptionAction(s, "cancel", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.OperationID = "unknown-provider-outcome"
	for _, action := range []string{"subscribe", "upgrade", "downgrade", "cancel", "refund_execute"} {
		if !errors.Is(CheckSubscriptionAction(s, action, time.Now()), ErrFinanceOperationInProgress) {
			t.Fatal(action, "bypassed unknown result")
		}
	}
}
