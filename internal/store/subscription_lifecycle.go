package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

func cloneLifecycle(v SubscriptionLifecycle) *SubscriptionLifecycle {
	if v.LastChangeAt != nil {
		t := *v.LastChangeAt
		v.LastChangeAt = &t
	}
	if v.EffectiveAt != nil {
		t := *v.EffectiveAt
		v.EffectiveAt = &t
	}
	return &v
}

func (s *memoryStore) GetSubscriptionLifecycle(_ context.Context, userID string) (*SubscriptionLifecycle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.byID[userID]; !ok {
		return nil, ErrUserNotFound
	}
	v := s.subscriptionLifecycles[userID]
	v.AccountID = userID
	for _, op := range s.financeOperations {
		if refundReviewPending(op, userID) {
			v.RefundReviewPending = true
			break
		}
	}
	return cloneLifecycle(v), nil
}

const lifecycleColumns = `account_uuid::text,version,last_change_at,COALESCE(operation_id::text,''),pending_plan_id,effective_at`

func scanLifecycle(row interface{ Scan(...any) error }) (*SubscriptionLifecycle, error) {
	v := new(SubscriptionLifecycle)
	err := row.Scan(&v.AccountID, &v.Version, &v.LastChangeAt, &v.OperationID, &v.PendingPlanID, &v.EffectiveAt)
	return v, err
}

func (s *postgresStore) GetSubscriptionLifecycle(ctx context.Context, userID string) (*SubscriptionLifecycle, error) {
	v, err := scanLifecycle(s.db.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM public.subscription_lifecycle_states WHERE account_uuid=$1`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.users WHERE uuid=$1)`, userID).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrUserNotFound
		}
		v = &SubscriptionLifecycle{AccountID: userID}
		err = nil
	}
	if err == nil {
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.finance_operations WHERE operation_type='refund_review' AND target_type='account' AND target_id=$1 AND status NOT IN ('succeeded','failed'))`, userID).Scan(&v.RefundReviewPending)
	}
	return v, err
}

func checkLifecyclePreviewVersion(request json.RawMessage, version int64) error {
	var quote struct {
		Version *int64 `json:"stateVersion"`
	}
	if len(request) > 0 && json.Unmarshal(request, &quote) != nil {
		return ErrSubscriptionStateConflict
	}
	if quote.Version != nil && *quote.Version != version {
		return ErrSubscriptionStateConflict
	}
	return nil
}

func actionOperation(userID, key, action string, request json.RawMessage) *FinanceOperation {
	return &FinanceOperation{ID: uuid.NewString(), IdempotencyKey: key, OperationType: "subscription_" + action, TargetType: "account", TargetID: userID, Provider: "stripe", Status: FinanceOperationInProgress, AttemptCount: 1, Request: financeJSON(request), Response: financeJSON(nil), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}

func sameAction(op *FinanceOperation, userID, key, action string, request json.RawMessage) bool {
	return op != nil && op.IdempotencyKey == key && op.TargetID == userID && op.TargetType == "account" && op.OperationType == "subscription_"+action && jsonEqual(op.Request, financeJSON(request))
}

func lifecycleAction(action string, request json.RawMessage) string {
	if action != "purchase" {
		return action
	}
	var quote struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(request, &quote) == nil && quote.Action == "upgrade" {
		return "upgrade"
	}
	return "subscribe"
}

func (s *memoryStore) ReserveSubscriptionAction(_ context.Context, userID, key, action string, request json.RawMessage) (*FinanceOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[userID]; !ok {
		return nil, ErrUserNotFound
	}
	if key == "" {
		return nil, ErrFinanceIdempotencyConflict
	}
	if id := s.financeOperationKeys[key]; id != "" {
		op := s.financeOperations[id]
		if !sameAction(op, userID, key, action, request) {
			return nil, ErrFinanceIdempotencyConflict
		}
		v := *op
		v.Request = financeJSON(op.Request)
		v.Response = financeJSON(op.Response)
		return &v, nil
	}
	v := s.subscriptionLifecycles[userID]
	v.AccountID = userID
	if err := checkLifecyclePreviewVersion(request, v.Version); err != nil {
		return nil, err
	}
	if err := CheckSubscriptionAction(v, lifecycleAction(action, request), time.Now().UTC()); err != nil {
		return nil, err
	}
	// Include operations created before the lifecycle migration.
	for _, op := range s.financeOperations {
		if purchasePending(op, userID) {
			return nil, ErrFinanceOperationInProgress
		}
		if (lifecycleAction(action, request) == "upgrade" || action == "downgrade") && refundReviewPending(op, userID) {
			return nil, ErrSubscriptionChangePending
		}
	}
	op := actionOperation(userID, key, action, request)
	s.financeOperations[op.ID] = op
	s.financeOperationKeys[key] = op.ID
	v.OperationID = op.ID
	v.Version++
	if s.subscriptionLifecycles == nil {
		s.subscriptionLifecycles = make(map[string]SubscriptionLifecycle)
	}
	s.subscriptionLifecycles[userID] = v
	s.financeOperationEvents[op.ID] = append(s.financeOperationEvents[op.ID], FinanceOperationEvent{ID: uuid.NewString(), OperationID: op.ID, Attempt: 1, EventType: "attempt_started", Status: op.Status, Payload: financeJSON(op.Request), OccurredAt: op.CreatedAt})
	copy := *op
	copy.Request = financeJSON(op.Request)
	return &copy, nil
}

func (s *postgresStore) ReserveSubscriptionAction(ctx context.Context, userID, key, action string, request json.RawMessage) (*FinanceOperation, error) {
	if key == "" {
		return nil, ErrFinanceIdempotencyConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "subscription-purchase:"+userID); err != nil {
		return nil, err
	}
	op, err := scanFinanceOperation(tx.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE idempotency_key=$1`, key))
	if err == nil {
		if !sameAction(op, userID, key, action, request) {
			return nil, ErrFinanceIdempotencyConflict
		}
		return op, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.subscription_lifecycle_states(account_uuid) SELECT uuid FROM public.users WHERE uuid=$1 ON CONFLICT DO NOTHING`, userID); err != nil {
		return nil, err
	}
	v, err := scanLifecycle(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM public.subscription_lifecycle_states WHERE account_uuid=$1 FOR UPDATE`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = checkLifecyclePreviewVersion(request, v.Version); err != nil {
		return nil, err
	}
	if err = CheckSubscriptionAction(*v, lifecycleAction(action, request), time.Now().UTC()); err != nil {
		return nil, err
	}
	var pending bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.finance_operations WHERE operation_type='subscription_purchase' AND target_type='account' AND target_id=$1 AND status NOT IN ('succeeded','failed'))`, userID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending {
		return nil, ErrFinanceOperationInProgress
	}
	if lifecycleAction(action, request) == "upgrade" || action == "downgrade" {
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.finance_operations WHERE operation_type='refund_review' AND target_type='account' AND target_id=$1 AND status NOT IN ('succeeded','failed'))`, userID).Scan(&pending); err != nil {
			return nil, err
		}
		if pending {
			return nil, ErrSubscriptionChangePending
		}
	}
	op = actionOperation(userID, key, action, request)
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.finance_operations(id,idempotency_key,operation_type,target_type,target_id,provider,status,attempt_count,request) VALUES($1,$2,$3,'account',$4,'stripe','in_progress',1,$5::jsonb)`, op.ID, key, op.OperationType, userID, []byte(op.Request)); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.subscription_lifecycle_states SET operation_id=$2,version=version+1,updated_at=now() WHERE account_uuid=$1`, userID, op.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.finance_operation_events(operation_id,attempt,event_type,status,payload) VALUES($1,1,'attempt_started','in_progress',$2::jsonb)`, op.ID, []byte(op.Request)); err != nil {
		return nil, err
	}
	return op, tx.Commit()
}

// confirmed means a verified successful provider result, NOT merely a 2xx or
// transport failure. confirmed=false releases only a proven unsubmitted failure.
func finishLifecycle(v *SubscriptionLifecycle, op *FinanceOperation, confirmed bool, pendingPlan string, effectiveAt *time.Time, now time.Time) error {
	if v.OperationID != op.ID {
		return ErrSubscriptionStateConflict
	}
	if (pendingPlan == "") != (effectiveAt == nil) {
		return ErrSubscriptionStateConflict
	}
	if !confirmed && pendingPlan != "" {
		return ErrSubscriptionStateConflict
	}
	if confirmed {
		if op.OperationType == "subscription_upgrade" || op.OperationType == "subscription_downgrade" || op.OperationType == "subscription_purchase" && lifecycleAction("purchase", op.Request) == "upgrade" {
			v.LastChangeAt = &now
		}
		if op.OperationType == "subscription_downgrade" || op.OperationType == "subscription_cancel" {
			v.PendingPlanID = pendingPlan
			v.EffectiveAt = effectiveAt
		}
	}
	v.OperationID = ""
	v.Version++
	return nil
}

func (s *memoryStore) FinishSubscriptionAction(_ context.Context, userID, id, providerID string, response json.RawMessage, confirmed bool, pendingPlan string, effectiveAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.financeOperations[id]
	if op == nil || op.TargetID != userID || op.TargetType != "account" {
		return ErrFinanceRecordNotFound
	}
	if op.ProviderOperationID != "" && op.ProviderOperationID != providerID {
		return ErrFinanceIdempotencyConflict
	}
	if op.Status == FinanceOperationSucceeded || op.Status == FinanceOperationFailed {
		if (op.Status == FinanceOperationSucceeded) != confirmed || !jsonEqual(op.Response, financeJSON(response)) {
			return ErrFinanceIdempotencyConflict
		}
		return nil
	}
	v := s.subscriptionLifecycles[userID]
	now := time.Now().UTC()
	if err := finishLifecycle(&v, op, confirmed, pendingPlan, effectiveAt, now); err != nil {
		return err
	}
	op.Status = FinanceOperationFailed
	if confirmed {
		op.Status = FinanceOperationSucceeded
	}
	op.ProviderOperationID = providerID
	op.Response = financeJSON(response)
	op.UpdatedAt = now
	s.subscriptionLifecycles[userID] = *cloneLifecycle(v)
	s.financeOperationEvents[id] = append(s.financeOperationEvents[id], FinanceOperationEvent{ID: uuid.NewString(), OperationID: id, Attempt: op.AttemptCount, EventType: "attempt_finished", Status: op.Status, ProviderOperationID: providerID, Payload: financeJSON(response), OccurredAt: now})
	return nil
}

func (s *postgresStore) FinishSubscriptionAction(ctx context.Context, userID, id, providerID string, response json.RawMessage, confirmed bool, pendingPlan string, effectiveAt *time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "subscription-purchase:"+userID); err != nil {
		return err
	}
	op, err := scanFinanceOperation(tx.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE id=$1 AND target_type='account' AND target_id=$2 FOR UPDATE`, id, userID))
	if err != nil {
		return err
	}
	if op.ProviderOperationID != "" && op.ProviderOperationID != providerID {
		return ErrFinanceIdempotencyConflict
	}
	if op.Status == FinanceOperationSucceeded || op.Status == FinanceOperationFailed {
		if (op.Status == FinanceOperationSucceeded) != confirmed || !jsonEqual(op.Response, financeJSON(response)) {
			return ErrFinanceIdempotencyConflict
		}
		return nil
	}
	v, err := scanLifecycle(tx.QueryRowContext(ctx, `SELECT `+lifecycleColumns+` FROM public.subscription_lifecycle_states WHERE account_uuid=$1 FOR UPDATE`, userID))
	if err != nil {
		return err
	}
	if err = finishLifecycle(v, op, confirmed, pendingPlan, effectiveAt, time.Now().UTC()); err != nil {
		return err
	}
	status := FinanceOperationFailed
	if confirmed {
		status = FinanceOperationSucceeded
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.finance_operations SET status=$2,provider_operation_id=NULLIF($3,''),response=$4::jsonb,updated_at=now() WHERE id=$1`, id, status, providerID, []byte(financeJSON(response))); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.subscription_lifecycle_states SET version=$2,last_change_at=$3,operation_id=NULL,pending_plan_id=$4,effective_at=$5,updated_at=now() WHERE account_uuid=$1`, userID, v.Version, v.LastChangeAt, v.PendingPlanID, v.EffectiveAt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.finance_operation_events(operation_id,attempt,event_type,status,provider_operation_id,payload) VALUES($1,$2,'attempt_finished',$3,NULLIF($4,''),$5::jsonb)`, id, op.AttemptCount, status, providerID, []byte(financeJSON(response))); err != nil {
		return err
	}
	return tx.Commit()
}
