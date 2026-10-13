package tasksession

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
)

func (s *PostgresStore) RenewLease(ctx context.Context, input RenewLeaseInput) (RenewLeaseResult, error) {
	input, err := normalizeRenew(input)
	if err != nil {
		return RenewLeaseResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RenewLeaseResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, leaseHash, err := loadTaskRunForUpdate(ctx, tx, input.TaskRunID, input.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return RenewLeaseResult{}, ErrNotFound
	}
	if err != nil {
		return RenewLeaseResult{}, err
	}
	if run.State != TaskRunRunning || run.Fence != input.Fence || run.LeaseOwner != input.WorkerID ||
		!run.LeaseExpires.After(input.Now) ||
		subtle.ConstantTimeCompare([]byte(leaseHash), []byte(hashLeaseToken(input.LeaseToken))) != 1 {
		return RenewLeaseResult{}, ErrLeaseConflict
	}
	expires := input.Now.Add(input.LeaseTTL)
	updated, err := tx.ExecContext(ctx, `UPDATE public.task_runs
SET lease_expires_at = $1, updated_at = $2
WHERE id = $3 AND account_uuid = $4 AND state = 'running' AND fence = $5 AND lease_token_hash = $6`,
		expires, input.Now, run.ID, input.AccountID, input.Fence, leaseHash)
	if err != nil {
		return RenewLeaseResult{}, err
	}
	if err := requireOneRow(updated); err != nil {
		return RenewLeaseResult{}, ErrLeaseConflict
	}
	cancelRequested, err := cancelRequestedFor(ctx, tx, run)
	if err != nil {
		return RenewLeaseResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RenewLeaseResult{}, err
	}
	run.LeaseExpires = expires
	run.UpdatedAt = input.Now
	return RenewLeaseResult{TaskRun: run, CancelRequested: cancelRequested}, nil
}

func cancelRequestedFor(ctx context.Context, tx *sql.Tx, run TaskRun) (bool, error) {
	var requested bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM public.task_session_events
  WHERE session_id = $1 AND event_type = $2 AND payload->>'taskRunId' = $3
)`, run.SessionID, EventRunCancelRequested, run.ID).Scan(&requested)
	return requested, err
}

func findCancelRequest(ctx context.Context, tx *sql.Tx, run TaskRun) (Event, error) {
	row := tx.QueryRowContext(ctx, `SELECT seq, event_type, payload, actor_id, client_request_id, created_at
FROM public.task_session_events
WHERE session_id = $1 AND event_type = $2 AND payload->>'taskRunId' = $3
ORDER BY seq
LIMIT 1`, run.SessionID, EventRunCancelRequested, run.ID)
	return scanEvent(row, run.SessionID)
}

func (s *PostgresStore) RequestCancel(ctx context.Context, input CancelRunInput) (TaskRunEventResult, error) {
	input, err := normalizeCancel(input)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := lockSession(ctx, tx, input.SessionID, input.AccountID)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	run, _, err := loadTaskRunForUpdate(ctx, tx, input.TaskRunID, input.AccountID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.SessionID != input.SessionID) {
		return TaskRunEventResult{}, ErrNotFound
	}
	if err != nil {
		return TaskRunEventResult{}, err
	}
	settled := func(event Event) (TaskRunEventResult, error) {
		if err := tx.Commit(); err != nil {
			return TaskRunEventResult{}, err
		}
		return TaskRunEventResult{Event: event, TaskRun: run, SnapshotVer: locked.snapshotVersion, LastEventSeq: locked.lastEventSeq}, nil
	}
	existing, err := findEventByRequest(ctx, tx, input.SessionID, input.ClientRequestID)
	if err == nil {
		if runID, _ := existing.Payload["taskRunId"].(string); runID != run.ID {
			return TaskRunEventResult{}, ErrAlreadyExists
		}
		return settled(existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TaskRunEventResult{}, err
	}

	var event Event
	switch {
	case runFinished(run.State):
		return TaskRunEventResult{}, ErrRunFinished
	case run.State == TaskRunQueued:
		updated, err := tx.ExecContext(ctx, `UPDATE public.task_runs
SET state = 'cancelled', updated_at = $1
WHERE id = $2 AND account_uuid = $3 AND state = 'queued'`, input.CreatedAt, run.ID, input.AccountID)
		if err != nil {
			return TaskRunEventResult{}, err
		}
		if err := requireOneRow(updated); err != nil {
			return TaskRunEventResult{}, err
		}
		run.State = TaskRunCanceled
		run.UpdatedAt = input.CreatedAt
		event = Event{Type: EventRunCancelled, Payload: cancelledBeforeStartPayload(run.ID)}
	default:
		requested, err := findCancelRequest(ctx, tx, run)
		if err == nil {
			return settled(requested)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return TaskRunEventResult{}, err
		}
		event = Event{Type: EventRunCancelRequested, Payload: cancelRequestedPayload(run.ID)}
	}
	payloadJSON, err := marshalEventPayload(event.Payload)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	event.SessionID = run.SessionID
	event.Seq = locked.lastEventSeq + 1
	event.ActorID = input.ActorID
	event.ClientRequestID = input.ClientRequestID
	event.CreatedAt = input.CreatedAt
	if err := insertEvent(ctx, tx, event, payloadJSON); err != nil {
		return TaskRunEventResult{}, err
	}
	contextSummary := cloneMap(locked.context)
	contextSummary["lastRunState"] = run.State
	if err := updateSessionAfterEvent(ctx, tx, input.AccountID, run.SessionID, event.Seq, 1, contextSummary, input.CreatedAt); err != nil {
		return TaskRunEventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskRunEventResult{}, err
	}
	return TaskRunEventResult{Event: event, TaskRun: run, SnapshotVer: locked.snapshotVersion + 1, LastEventSeq: event.Seq}, nil
}

func (s *PostgresStore) RunMessage(ctx context.Context, accountID, taskRunID string) (RunMessage, error) {
	accountID = strings.TrimSpace(accountID)
	taskRunID = strings.TrimSpace(taskRunID)
	if accountID == "" || taskRunID == "" {
		return RunMessage{}, ErrInvalidInput
	}
	var routingJSON, payloadJSON []byte
	err := s.db.QueryRowContext(ctx, `SELECT run.routing, event.payload
FROM public.task_runs AS run
JOIN public.task_session_events AS event
  ON event.session_id = run.session_id AND event.event_type = $3 AND event.payload->>'taskRunId' = run.id
WHERE run.id = $1 AND run.account_uuid = $2
ORDER BY event.seq
LIMIT 1`, taskRunID, accountID, EventMessageCreated).Scan(&routingJSON, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return RunMessage{}, ErrNotFound
	}
	if err != nil {
		return RunMessage{}, err
	}
	routing, err := decodeObject(routingJSON)
	if err != nil {
		return RunMessage{}, err
	}
	payload, err := decodeObject(payloadJSON)
	if err != nil {
		return RunMessage{}, err
	}
	text, _ := payload["text"].(string)
	return RunMessage{Text: text, Routing: routing}, nil
}
