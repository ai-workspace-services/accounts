package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

func refundReviewPending(op *FinanceOperation, userID string) bool {
	return op.OperationType == "refund_review" && op.TargetID == userID && op.TargetType == "account" && op.Status != FinanceOperationSucceeded && op.Status != FinanceOperationFailed
}

func validReview(op *FinanceOperation) bool {
	return op != nil && op.OperationType == "refund_review" && op.TargetType == "account" && op.TargetID != "" && op.Provider == "stripe" && op.IdempotencyKey == "subscription-refund-review:"+op.TargetID
}

func (s *memoryStore) CreateSubscriptionRefundReview(_ context.Context, op *FinanceOperation, expectedVersion int64) error {
	if !validReview(op) {
		return ErrFinanceIdempotencyConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[op.TargetID]; !ok {
		return ErrUserNotFound
	}
	if id := s.financeOperationKeys[op.IdempotencyKey]; id != "" {
		saved := s.financeOperations[id]
		*op = *saved
		op.Request = financeJSON(saved.Request)
		return nil
	}
	v := s.subscriptionLifecycles[op.TargetID]
	if v.OperationID != "" || v.Version != expectedVersion {
		return ErrSubscriptionStateConflict
	}
	for _, other := range s.financeOperations {
		if purchasePending(other, op.TargetID) {
			return ErrFinanceOperationInProgress
		}
	}
	now := time.Now().UTC()
	copy := *op
	copy.ID = uuid.NewString()
	copy.Request = financeJSON(op.Request)
	copy.Response = financeJSON(nil)
	copy.Status = FinanceOperationInProgress
	copy.AttemptCount = 1
	copy.CreatedAt = now
	copy.UpdatedAt = now
	s.financeOperations[copy.ID] = &copy
	s.financeOperationKeys[copy.IdempotencyKey] = copy.ID
	s.financeOperationEvents[copy.ID] = append(s.financeOperationEvents[copy.ID], FinanceOperationEvent{ID: uuid.NewString(), OperationID: copy.ID, Attempt: 1, EventType: "review_requested", Status: copy.Status, Payload: financeJSON(copy.Request), OccurredAt: now})
	v.AccountID = op.TargetID
	v.Version++
	if s.subscriptionLifecycles == nil {
		s.subscriptionLifecycles = make(map[string]SubscriptionLifecycle)
	}
	s.subscriptionLifecycles[op.TargetID] = v
	*op = copy
	op.Request = financeJSON(copy.Request)
	return nil
}

func (s *postgresStore) CreateSubscriptionRefundReview(ctx context.Context, op *FinanceOperation, expectedVersion int64) error {
	if !validReview(op) {
		return ErrFinanceIdempotencyConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "subscription-purchase:"+op.TargetID); err != nil {
		return err
	}
	existing, err := scanFinanceOperation(tx.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE idempotency_key=$1`, op.IdempotencyKey))
	if err == nil {
		if !validReview(existing) {
			return ErrFinanceIdempotencyConflict
		}
		*op = *existing
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.subscription_lifecycle_states(account_uuid) SELECT uuid FROM public.users WHERE uuid=$1 ON CONFLICT DO NOTHING`, op.TargetID); err != nil {
		return err
	}
	v, err := scanLifecycle(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM public.subscription_lifecycle_states WHERE account_uuid=$1 FOR UPDATE`, op.TargetID))
	if err != nil {
		return err
	}
	if v.OperationID != "" || v.Version != expectedVersion {
		return ErrSubscriptionStateConflict
	}
	var pending bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.finance_operations WHERE operation_type='subscription_purchase' AND target_type='account' AND target_id=$1 AND status NOT IN ('succeeded','failed'))`, op.TargetID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrFinanceOperationInProgress
	}
	saved, err := scanFinanceOperation(tx.QueryRowContext(ctx, `INSERT INTO public.finance_operations(id,idempotency_key,operation_type,target_type,target_id,provider,status,attempt_count,request) VALUES($1,$2,'refund_review','account',$3,'stripe','in_progress',1,$4::jsonb) RETURNING `+financeOperationColumns, uuid.NewString(), op.IdempotencyKey, op.TargetID, []byte(financeJSON(op.Request))))
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.subscription_lifecycle_states SET version=version+1,updated_at=now() WHERE account_uuid=$1`, op.TargetID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.finance_operation_events(operation_id,attempt,event_type,status,payload) VALUES($1,1,'review_requested','in_progress',$2::jsonb)`, saved.ID, []byte(saved.Request)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	*op = *saved
	return nil
}
