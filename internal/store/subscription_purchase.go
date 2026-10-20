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
func (s *memoryStore) ReserveSubscriptionPurchase(ctx context.Context, userID string, request json.RawMessage) (*FinanceOperation, error) {
	return s.ReserveSubscriptionAction(ctx, userID, "subscription-purchase:"+uuid.NewString(), "purchase", request)
}
func (s *memoryStore) UpdateSubscriptionPurchase(ctx context.Context, id, providerID string, response json.RawMessage, complete bool) error {
	if complete {
		s.mu.RLock()
		op := s.financeOperations[id]
		var userID string
		if op != nil {
			userID = op.TargetID
		}
		s.mu.RUnlock()
		if userID == "" {
			return ErrFinanceRecordNotFound
		}
		var receipt struct {
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(response, &receipt)
		return s.FinishSubscriptionAction(ctx, userID, id, providerID, response, receipt.Outcome == "webhook_confirmed", "", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.financeOperations[id]
	if op == nil || op.OperationType != subscriptionPurchaseType {
		return ErrFinanceRecordNotFound
	}
	if op.ProviderOperationID != "" && op.ProviderOperationID != providerID {
		return ErrFinanceIdempotencyConflict
	}
	if op.Status == FinanceOperationSucceeded || op.Status == FinanceOperationFailed {
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
	return s.ReserveSubscriptionAction(ctx, userID, "subscription-purchase:"+uuid.NewString(), "purchase", request)
}
func (s *postgresStore) UpdateSubscriptionPurchase(ctx context.Context, id, providerID string, response json.RawMessage, complete bool) error {
	if complete {
		var userID string
		if err := s.db.QueryRowContext(ctx, `SELECT target_id FROM public.finance_operations WHERE id=$1 AND operation_type='subscription_purchase'`, id).Scan(&userID); err != nil {
			return err
		}
		var receipt struct {
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(response, &receipt)
		return s.FinishSubscriptionAction(ctx, userID, id, providerID, response, receipt.Outcome == "webhook_confirmed", "", nil)
	}
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
		if (status != FinanceOperationSucceeded && status != FinanceOperationFailed) || savedProvider.String != providerID {
			return ErrFinanceIdempotencyConflict
		}
	}
	return nil
}
