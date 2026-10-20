package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubscriptionActionSharedGateAndCooldown(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	u := &User{Name: "Lifecycle", Email: "lifecycle@example.invalid"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var winner *FinanceOperation
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, action := range []string{"upgrade", "downgrade", "cancel", "refund_execute"} {
		wg.Add(1)
		go func(action string) {
			defer wg.Done()
			op, err := st.ReserveSubscriptionAction(ctx, u.ID, "operation:"+action, action, json.RawMessage(`{"revision":0}`))
			if err == nil {
				winners.Add(1)
				mu.Lock()
				winner = op
				mu.Unlock()
			} else if !errors.Is(err, ErrFinanceOperationInProgress) {
				t.Error(err)
			}
		}(action)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("gate winners", winners.Load())
	}
	state, err := st.GetSubscriptionLifecycle(ctx, u.ID)
	if err != nil || state.OperationID != winner.ID || state.Version != 1 {
		t.Fatal(state, err)
	}
	if err := st.FinishSubscriptionAction(ctx, u.ID, winner.ID, "", json.RawMessage(`{"outcome":"not_submitted"}`), false, "", nil); err != nil {
		t.Fatal(err)
	}
	state, _ = st.GetSubscriptionLifecycle(ctx, u.ID)
	if state.LastChangeAt != nil || state.OperationID != "" {
		t.Fatal("failed pre-submit consumed quota")
	}
	op, err := st.ReserveSubscriptionAction(ctx, u.ID, "upgrade:confirmed", "upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSubscriptionAction(ctx, u.ID, op.ID, "in_paid", json.RawMessage(`{"paid":true}`), true, "", nil); err != nil {
		t.Fatal(err)
	}
	state, _ = st.GetSubscriptionLifecycle(ctx, u.ID)
	if state.LastChangeAt == nil || state.CooldownUntil().Sub(*state.LastChangeAt) != 30*24*time.Hour {
		t.Fatal("cooldown missing")
	}
	if _, err = st.ReserveSubscriptionAction(ctx, u.ID, "different-subscription", "downgrade", nil); !errors.Is(err, ErrSubscriptionCooldown) {
		t.Fatal("subscription ID reset gate", err)
	}
	// Cancellation is independent and replay returns the exact original op.
	cancel, err := st.ReserveSubscriptionAction(ctx, u.ID, "cancel:original", "cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Hour)
	receipt := json.RawMessage(`{"scheduled":true}`)
	if err = st.FinishSubscriptionAction(ctx, u.ID, cancel.ID, "sub_original", receipt, true, "FREE", &end); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetSubscriptionLifecycle(ctx, u.ID)
	if err = st.FinishSubscriptionAction(ctx, u.ID, cancel.ID, "sub_original", receipt, true, "FREE", &end); err != nil {
		t.Fatal(err)
	}
	again, err := st.ReserveSubscriptionAction(ctx, u.ID, "cancel:original", "cancel", nil)
	if err != nil || again.ID != cancel.ID {
		t.Fatal(again, err)
	}
	after, _ := st.GetSubscriptionLifecycle(ctx, u.ID)
	if after.Version != before.Version || !after.LastChangeAt.Equal(*state.LastChangeAt) {
		t.Fatal("replay changed cooldown/version")
	}
	if _, err = st.ReserveSubscriptionAction(ctx, u.ID, "new-sub", "subscribe", nil); !errors.Is(err, ErrSubscriptionChangePending) {
		t.Fatal("pending cancellation allowed repurchase", err)
	}
}

func TestRefundReviewSnapshotCASAndSharedGate(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	u := &User{Name: "Review", Email: "review-gate@example.invalid"}
	_ = st.CreateUser(ctx, u)
	op, err := st.ReserveSubscriptionAction(ctx, u.ID, "upgrade", "upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	review := &FinanceOperation{IdempotencyKey: "subscription-refund-review:" + u.ID, OperationType: "refund_review", TargetType: "account", TargetID: u.ID, Provider: "stripe", Request: json.RawMessage(`{"requestedAt":"first"}`)}
	if err := st.CreateSubscriptionRefundReview(ctx, review, 0); !errors.Is(err, ErrSubscriptionStateConflict) {
		t.Fatal("review raced operation", err)
	}
	if err := st.FinishSubscriptionAction(ctx, u.ID, op.ID, "", nil, false, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSubscriptionRefundReview(ctx, review, 0); !errors.Is(err, ErrSubscriptionStateConflict) {
		t.Fatal("stale evidence accepted", err)
	}
	state, _ := st.GetSubscriptionLifecycle(ctx, u.ID)
	if err := st.CreateSubscriptionRefundReview(ctx, review, state.Version); err != nil {
		t.Fatal(err)
	}
	id := review.ID
	review.Request = json.RawMessage(`{"requestedAt":"later"}`)
	if err := st.CreateSubscriptionRefundReview(ctx, review, 0); err != nil || review.ID != id || string(review.Request) != `{"requestedAt":"first"}` {
		t.Fatal("application window reset", err)
	}
	for _, action := range []string{"upgrade", "downgrade"} {
		if _, err := st.ReserveSubscriptionAction(ctx, u.ID, "blocked:"+action, action, nil); !errors.Is(err, ErrSubscriptionChangePending) {
			t.Fatal("review bypass", action, err)
		}
	}
	if _, err := st.ReserveSubscriptionAction(ctx, u.ID, "cancel", "cancel", nil); err != nil {
		t.Fatal("review blocked cancellation", err)
	}
}
