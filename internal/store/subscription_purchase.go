package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const subscriptionPurchaseType = "subscription_purchase"

func purchasePending(op *FinanceOperation, userID string) bool {
	return op.OperationType == subscriptionPurchaseType && op.TargetID == userID && op.TargetType == "account" && op.Status != FinanceOperationSucceeded && op.Status != FinanceOperationFailed
}

func (s *memoryStore) PendingSubscriptionPurchase(_ context.Context, userID string) (*FinanceOperation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, op := range s.financeOperations {
		if purchasePending(op, userID) {
			copy := *op
			copy.Request = financeJSON(op.Request)
			copy.Response = financeJSON(op.Response)
			return &copy, nil
		}
	}
	return nil, nil
}
func (s *memoryStore) ReserveSubscriptionPurchase(_ context.Context, userID string, request json.RawMessage) (*FinanceOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[userID]; !ok {
		return nil, ErrUserNotFound
	}
	for _, op := range s.financeOperations {
		if purchasePending(op, userID) {
			return nil, ErrFinanceOperationInProgress
		}
	}
	now := time.Now().UTC()
	id := uuid.NewString()
	op := &FinanceOperation{ID: id, IdempotencyKey: "subscription-purchase:" + id, OperationType: subscriptionPurchaseType, TargetType: "account", TargetID: userID, Provider: "stripe", Status: FinanceOperationInProgress, AttemptCount: 1, Request: financeJSON(request), Response: financeJSON(nil), CreatedAt: now, UpdatedAt: now}
	s.financeOperations[id] = op
	s.financeOperationKeys[op.IdempotencyKey] = id
	copy := *op
	return &copy, nil
}
func (s *memoryStore) UpdateSubscriptionPurchase(_ context.Context, id, providerID string, response json.RawMessage, complete bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.financeOperations[id]
	if op == nil || op.OperationType != subscriptionPurchaseType {
		return ErrFinanceRecordNotFound
	}
	if op.ProviderOperationID != "" && op.ProviderOperationID != providerID {
		return ErrFinanceIdempotencyConflict
	}
	if op.Status == FinanceOperationSucceeded {
		return nil
	}
	op.ProviderOperationID = providerID
	op.Response = financeJSON(response)
	op.Status = FinanceOperationReconciliationRequired
	if complete {
		op.Status = FinanceOperationSucceeded
	}
	op.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *postgresStore) PendingSubscriptionPurchase(ctx context.Context, userID string) (*FinanceOperation, error) {
	op, err := scanFinanceOperation(s.db.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE operation_type='subscription_purchase' AND target_type='account' AND target_id=$1 AND status NOT IN ('succeeded','failed') ORDER BY created_at LIMIT 1`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return op, err
}
func (s *postgresStore) ReserveSubscriptionPurchase(ctx context.Context, userID string, request json.RawMessage) (*FinanceOperation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Account-scoped transaction lock serializes different plans and quote IDs.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "subscription-purchase:"+userID); err != nil {
		return nil, err
	}
	var userExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.users WHERE uuid=$1)`, userID).Scan(&userExists); err != nil {
		return nil, err
	}
	if !userExists {
		return nil, ErrUserNotFound
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.finance_operations WHERE operation_type='subscription_purchase' AND target_type='account' AND target_id=$1 AND status NOT IN ('succeeded','failed'))`, userID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrFinanceOperationInProgress
	}
	id := uuid.NewString()
	op, err := scanFinanceOperation(tx.QueryRowContext(ctx, `INSERT INTO public.finance_operations (id,idempotency_key,operation_type,target_type,target_id,provider,status,attempt_count,request) VALUES ($1::uuid,$2,'subscription_purchase','account',$3,'stripe','in_progress',1,$4::jsonb) RETURNING `+financeOperationColumns, id, "subscription-purchase:"+id, userID, []byte(financeJSON(request))))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return op, nil
}
func (s *postgresStore) UpdateSubscriptionPurchase(ctx context.Context, id, providerID string, response json.RawMessage, complete bool) error {
	status := FinanceOperationReconciliationRequired
	if complete {
		status = FinanceOperationSucceeded
	}
	result, err := s.db.ExecContext(ctx, `UPDATE public.finance_operations SET provider_operation_id=NULLIF($2,''),response=$3::jsonb,status=$4,updated_at=now() WHERE id=$1::uuid AND operation_type='subscription_purchase' AND status NOT IN ('succeeded','failed') AND (provider_operation_id IS NULL OR provider_operation_id=$2)`, id, providerID, []byte(financeJSON(response)), status)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var status string
		var savedProvider sql.NullString
		if err = s.db.QueryRowContext(ctx, `SELECT status,provider_operation_id FROM public.finance_operations WHERE id=$1::uuid AND operation_type='subscription_purchase'`, id).Scan(&status, &savedProvider); err != nil {
			return err
		}
		if status != FinanceOperationSucceeded || savedProvider.String != providerID {
			return ErrFinanceIdempotencyConflict
		}
	}
	return nil
}
