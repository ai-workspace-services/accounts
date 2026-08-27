package tasksession

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type PostgresStore struct {
	db *sql.DB
}

var _ Store = (*PostgresStore)(nil)

func NewPostgresStore(db *sql.DB) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("task session database is nil")
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) EnsurePersonalNamespace(ctx context.Context, accountID string, now time.Time) (Namespace, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return Namespace{}, ErrAccountMismatch
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	row := s.db.QueryRowContext(ctx, `INSERT INTO public.task_namespaces
  (id, account_uuid, slug, display_name, max_active_runs, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_uuid, slug) DO UPDATE
SET account_uuid = EXCLUDED.account_uuid
RETURNING id, account_uuid::text, slug, display_name, max_active_runs, created_at`,
		uuid.NewString(), accountID, NamespacePersonal, "Personal", defaultNamespaceMaxActive, now)
	var namespace Namespace
	if err := row.Scan(&namespace.ID, &namespace.AccountID, &namespace.Slug, &namespace.DisplayName, &namespace.MaxActiveRuns, &namespace.CreatedAt); err != nil {
		return Namespace{}, mapPostgresError(err)
	}
	return namespace, nil
}

func (s *PostgresStore) CreateNamespace(ctx context.Context, input CreateNamespaceInput) (Namespace, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.Slug = strings.TrimSpace(input.Slug)
	if input.ID == "" {
		input.ID = uuid.NewString()
	}
	if input.AccountID == "" || input.Slug == "" {
		return Namespace{}, ErrInvalidInput
	}
	if input.MaxActiveRuns <= 0 || input.MaxActiveRuns > defaultNamespaceMaxActive {
		input.MaxActiveRuns = defaultNamespaceMaxActive
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	row := s.db.QueryRowContext(ctx, `INSERT INTO public.task_namespaces
  (id, account_uuid, slug, display_name, max_active_runs, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, account_uuid::text, slug, display_name, max_active_runs, created_at`,
		input.ID, input.AccountID, input.Slug, strings.TrimSpace(input.DisplayName), input.MaxActiveRuns, input.CreatedAt)
	var namespace Namespace
	if err := row.Scan(&namespace.ID, &namespace.AccountID, &namespace.Slug, &namespace.DisplayName, &namespace.MaxActiveRuns, &namespace.CreatedAt); err != nil {
		return Namespace{}, mapPostgresError(err)
	}
	return namespace, nil
}

func (s *PostgresStore) ListNamespaces(ctx context.Context, accountID string) ([]Namespace, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_uuid::text, slug, display_name, max_active_runs, created_at
FROM public.task_namespaces
WHERE account_uuid = $1
ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Namespace, 0)
	for rows.Next() {
		var item Namespace
		if err := rows.Scan(&item.ID, &item.AccountID, &item.Slug, &item.DisplayName, &item.MaxActiveRuns, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) CreateSession(ctx context.Context, input CreateSessionInput) (Session, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.NamespaceID = strings.TrimSpace(input.NamespaceID)
	if input.ID == "" {
		input.ID = uuid.NewString()
	}
	if input.AccountID == "" || input.NamespaceID == "" {
		return Session{}, ErrInvalidInput
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	row := s.db.QueryRowContext(ctx, `INSERT INTO public.task_sessions
  (id, account_uuid, namespace_id, title, lifecycle_state, context_summary, created_at, updated_at)
SELECT $1, $2, namespace.id, $4, $5, '{}'::jsonb, $6, $6
FROM public.task_namespaces AS namespace
WHERE namespace.id = $3 AND namespace.account_uuid = $2
RETURNING id, account_uuid::text, namespace_id, title, snapshot_version, last_event_seq,
  lifecycle_state, context_summary, created_at, updated_at`,
		input.ID, input.AccountID, input.NamespaceID, strings.TrimSpace(input.Title), SessionReady, input.CreatedAt)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrAccountMismatch
	}
	if err != nil {
		return Session{}, mapPostgresError(err)
	}
	return session, nil
}

func (s *PostgresStore) ListSessions(ctx context.Context, accountID, namespaceID string) ([]Snapshot, error) {
	accountID = strings.TrimSpace(accountID)
	namespaceID = strings.TrimSpace(namespaceID)
	if accountID == "" || namespaceID == "" {
		return nil, ErrInvalidInput
	}
	var ownsNamespace bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM public.task_namespaces WHERE id = $1 AND account_uuid = $2
)`, namespaceID, accountID).Scan(&ownsNamespace); err != nil {
		return nil, err
	}
	if !ownsNamespace {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, snapshotSelect+`
WHERE session.account_uuid = $1 AND session.namespace_id = $2
ORDER BY session.updated_at DESC, session.id`, accountID, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Snapshot, 0)
	for rows.Next() {
		item, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) GetSession(ctx context.Context, accountID, namespaceID, sessionID string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, account_uuid::text, namespace_id, title,
  snapshot_version, last_event_seq, lifecycle_state, context_summary, created_at, updated_at
FROM public.task_sessions
WHERE id = $1 AND account_uuid = $2 AND namespace_id = $3`,
		strings.TrimSpace(sessionID), strings.TrimSpace(accountID), strings.TrimSpace(namespaceID))
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *PostgresStore) AppendEvent(ctx context.Context, input AppendEventInput) (Event, error) {
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	input.Type = strings.TrimSpace(input.Type)
	if input.AccountID == "" || input.SessionID == "" || input.Type == "" {
		return Event{}, ErrInvalidInput
	}
	payload, err := marshalEventPayload(input.Payload)
	if err != nil {
		return Event{}, err
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := lockSession(ctx, tx, input.SessionID, input.AccountID)
	if err != nil {
		return Event{}, err
	}
	if input.ClientRequestID != "" {
		existing, err := findEventByRequest(ctx, tx, input.SessionID, input.ClientRequestID)
		if err == nil {
			if err := tx.Commit(); err != nil {
				return Event{}, err
			}
			return existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Event{}, err
		}
	}
	event := Event{
		SessionID: input.SessionID, Seq: locked.lastEventSeq + 1, Type: input.Type,
		Payload: cloneMap(input.Payload), ActorID: input.ActorID,
		ClientRequestID: input.ClientRequestID, CreatedAt: input.CreatedAt,
	}
	if err := insertEvent(ctx, tx, event, payload); err != nil {
		return Event{}, err
	}
	contextSummary := cloneMap(locked.context)
	updateContextSummary(contextSummary, input.Payload)
	encodedContext, err := marshalContextSummary(contextSummary)
	if err != nil {
		return Event{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE public.task_sessions
SET last_event_seq = $1, snapshot_version = snapshot_version + 1,
  context_summary = $2, updated_at = $3
WHERE id = $4 AND account_uuid = $5`, event.Seq, encodedContext, input.CreatedAt, input.SessionID, input.AccountID)
	if err != nil {
		return Event{}, err
	}
	if err := requireOneRow(result); err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (s *PostgresStore) AppendMessage(ctx context.Context, input AppendMessageInput) (MessageCommandResult, error) {
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	input.Text = strings.TrimSpace(input.Text)
	input.TaskRunID = strings.TrimSpace(input.TaskRunID)
	if input.AccountID == "" || input.SessionID == "" || input.ClientRequestID == "" || input.Text == "" {
		return MessageCommandResult{}, ErrInvalidInput
	}
	if input.ActorID == "" {
		input.ActorID = input.AccountID
	}
	if input.TaskRunID == "" {
		input.TaskRunID = uuid.NewString()
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	if input.NotBefore.IsZero() {
		input.NotBefore = input.CreatedAt
	}
	messagePayload, err := messageEventPayload(input.Text, input.TaskRunID)
	if err != nil {
		return MessageCommandResult{}, err
	}
	messageJSON, err := marshalEventPayload(messagePayload)
	if err != nil {
		return MessageCommandResult{}, err
	}
	queuedPayload := runQueuedEventPayload(input.TaskRunID)
	queuedJSON, err := marshalEventPayload(queuedPayload)
	if err != nil {
		return MessageCommandResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageCommandResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := lockSession(ctx, tx, input.SessionID, input.AccountID)
	if err != nil {
		return MessageCommandResult{}, err
	}
	existing, err := findEventByRequest(ctx, tx, input.SessionID, input.ClientRequestID)
	if err == nil {
		result, err := loadExistingMessageCommand(ctx, tx, input.AccountID, locked, existing)
		if err != nil {
			return MessageCommandResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return MessageCommandResult{}, err
		}
		result.Existing = true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MessageCommandResult{}, err
	}

	message := Event{
		SessionID: input.SessionID, Seq: locked.lastEventSeq + 1, Type: EventMessageCreated,
		Payload: messagePayload, ActorID: input.ActorID, ClientRequestID: input.ClientRequestID,
		CreatedAt: input.CreatedAt,
	}
	if err := insertEvent(ctx, tx, message, messageJSON); err != nil {
		return MessageCommandResult{}, err
	}
	run := TaskRun{
		ID: input.TaskRunID, AccountID: input.AccountID, NamespaceID: locked.namespaceID,
		SessionID: input.SessionID, ClientRequestID: input.ClientRequestID, State: TaskRunQueued,
		Priority: input.Priority, NotBefore: input.NotBefore, CreatedAt: input.CreatedAt, UpdatedAt: input.CreatedAt,
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO public.task_runs
  (id, account_uuid, namespace_id, session_id, client_request_id, state, priority, not_before, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'queued', $6, $7, $8, $8)`,
		run.ID, run.AccountID, run.NamespaceID, run.SessionID, run.ClientRequestID, run.Priority, run.NotBefore, run.CreatedAt)
	if err != nil {
		return MessageCommandResult{}, mapPostgresError(err)
	}
	if err := requireOneRow(result); err != nil {
		return MessageCommandResult{}, err
	}
	queued := Event{
		SessionID: input.SessionID, Seq: message.Seq + 1, Type: EventRunQueued,
		Payload: queuedPayload, ActorID: input.ActorID, CreatedAt: input.CreatedAt,
	}
	if err := insertEvent(ctx, tx, queued, queuedJSON); err != nil {
		return MessageCommandResult{}, err
	}
	contextSummary := cloneMap(locked.context)
	contextSummary["lastMessage"] = input.Text
	contextSummary, encodedContext, err := appendMessageToContext(contextSummary, messageContextValue(input.Text, input.TaskRunID, input.CreatedAt))
	if err != nil {
		return MessageCommandResult{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE public.task_sessions
SET last_event_seq = $1, snapshot_version = snapshot_version + 2,
  context_summary = $2, updated_at = $3
WHERE id = $4 AND account_uuid = $5`, queued.Seq, encodedContext, input.CreatedAt, input.SessionID, input.AccountID)
	if err != nil {
		return MessageCommandResult{}, err
	}
	if err := requireOneRow(updated); err != nil {
		return MessageCommandResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageCommandResult{}, err
	}
	return MessageCommandResult{
		Message: message, Queued: queued, TaskRun: run,
		SnapshotVer: locked.snapshotVersion + 2, LastEventSeq: queued.Seq,
	}, nil
}

func (s *PostgresStore) GetSnapshot(ctx context.Context, accountID, sessionID string) (Snapshot, error) {
	snapshot, err := scanSnapshot(s.db.QueryRowContext(ctx, snapshotSelect+`
WHERE session.id = $1 AND session.account_uuid = $2`, strings.TrimSpace(sessionID), strings.TrimSpace(accountID)))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

const snapshotSelect = `SELECT session.id, session.namespace_id, session.title,
  session.snapshot_version, session.last_event_seq, session.lifecycle_state,
  session.context_summary, session.updated_at,
  run.id, run.state, run.bridge_task_ref, run.priority, run.not_before,
  run.created_at, run.updated_at
FROM public.task_sessions AS session
LEFT JOIN LATERAL (
  SELECT id, state, bridge_task_ref, priority, not_before, created_at, updated_at
  FROM public.task_runs
  WHERE session_id = session.id AND account_uuid = session.account_uuid
  ORDER BY updated_at DESC, created_at DESC, id DESC
  LIMIT 1
) AS run ON TRUE`

func scanSnapshot(row scanner) (Snapshot, error) {
	var (
		snapshot      Snapshot
		contextJSON   []byte
		runID         sql.NullString
		runState      sql.NullString
		bridgeTaskRef sql.NullString
		priority      sql.NullInt64
		notBefore     sql.NullTime
		runCreatedAt  sql.NullTime
		runUpdatedAt  sql.NullTime
	)
	if err := row.Scan(
		&snapshot.SessionID, &snapshot.NamespaceID, &snapshot.Title,
		&snapshot.SnapshotVer, &snapshot.LastEventSeq, &snapshot.LifecycleState,
		&contextJSON, &snapshot.UpdatedAt, &runID, &runState, &bridgeTaskRef,
		&priority, &notBefore, &runCreatedAt, &runUpdatedAt,
	); err != nil {
		return Snapshot{}, err
	}
	contextSummary, err := decodeObject(contextJSON)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Context = contextSummary
	if runID.Valid {
		run := TaskRun{
			ID: runID.String, NamespaceID: snapshot.NamespaceID,
			SessionID: snapshot.SessionID, State: runState.String,
			BridgeRef: bridgeTaskRef.String, Priority: int(priority.Int64),
		}
		if notBefore.Valid {
			run.NotBefore = notBefore.Time
		}
		if runCreatedAt.Valid {
			run.CreatedAt = runCreatedAt.Time
		}
		if runUpdatedAt.Valid {
			run.UpdatedAt = runUpdatedAt.Time
		}
		snapshot.TaskRun = &run
	}
	return snapshot, nil
}

func (s *PostgresStore) ListEvents(ctx context.Context, accountID, sessionID string, afterSeq int64, limit int) ([]Event, error) {
	accountID = strings.TrimSpace(accountID)
	sessionID = strings.TrimSpace(sessionID)
	if accountID == "" || sessionID == "" {
		return nil, ErrInvalidInput
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM public.task_sessions WHERE id = $1 AND account_uuid = $2
)`, sessionID, accountID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event.seq, event.event_type, event.payload,
  event.actor_id, event.client_request_id, event.created_at
FROM public.task_session_events AS event
WHERE event.session_id = $1 AND event.seq > $2
ORDER BY event.seq
LIMIT $3`, sessionID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0, limit)
	for rows.Next() {
		event, err := scanEvent(rows, sessionID)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *PostgresStore) EnqueueTaskRun(ctx context.Context, input EnqueueTaskRunInput) (TaskRun, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.NamespaceID = strings.TrimSpace(input.NamespaceID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	if input.ID == "" {
		input.ID = uuid.NewString()
	}
	if input.AccountID == "" || input.NamespaceID == "" || input.SessionID == "" {
		return TaskRun{}, ErrInvalidInput
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	if input.NotBefore.IsZero() {
		input.NotBefore = input.CreatedAt
	}
	row := s.db.QueryRowContext(ctx, `INSERT INTO public.task_runs
  (id, account_uuid, namespace_id, session_id, client_request_id, state, priority, not_before, created_at, updated_at)
SELECT $1, $2, session.namespace_id, session.id, $5, 'queued', $6, $7, $8, $8
FROM public.task_sessions AS session
WHERE session.id = $4 AND session.account_uuid = $2 AND session.namespace_id = $3
RETURNING id, account_uuid::text, namespace_id, session_id, client_request_id, state,
  priority, not_before, attempt, lease_owner, lease_token_hash, lease_expires_at,
  fence, bridge_task_ref, created_at, updated_at`,
		input.ID, input.AccountID, input.NamespaceID, input.SessionID, input.ClientRequestID,
		input.Priority, input.NotBefore, input.CreatedAt)
	run, _, err := scanTaskRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRun{}, ErrAccountMismatch
	}
	if err != nil {
		return TaskRun{}, mapPostgresError(err)
	}
	return run, nil
}

func (s *PostgresStore) ClaimNext(ctx context.Context, input ClaimInput) (TaskRun, error) {
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	if input.AccountID == "" || input.WorkerID == "" {
		return TaskRun{}, ErrInvalidInput
	}
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	if input.MaxGlobalActive <= 0 || input.MaxGlobalActive > defaultGlobalMaxActive {
		input.MaxGlobalActive = defaultGlobalMaxActive
	}
	if input.LeaseTTL <= 0 {
		input.LeaseTTL = time.Minute
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskRun{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize quota decisions for one account while still allowing different
	// accounts to claim concurrently. SKIP LOCKED below handles worker races.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, input.AccountID); err != nil {
		return TaskRun{}, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*)
FROM public.task_runs
WHERE account_uuid = $1 AND state = 'running' AND lease_expires_at > $2`, input.AccountID, input.Now).Scan(&active); err != nil {
		return TaskRun{}, err
	}
	if active >= input.MaxGlobalActive {
		return TaskRun{}, ErrNoEligibleTask
	}

	run, _, err := scanTaskRun(tx.QueryRowContext(ctx, `SELECT run.id, run.account_uuid::text,
  run.namespace_id, run.session_id, run.client_request_id, run.state, run.priority,
  run.not_before, run.attempt, run.lease_owner, run.lease_token_hash,
  run.lease_expires_at, run.fence, run.bridge_task_ref, run.created_at, run.updated_at
FROM public.task_runs AS run
JOIN public.task_namespaces AS namespace ON namespace.id = run.namespace_id
WHERE run.account_uuid = $1
  AND (
    (run.state = 'queued' AND run.not_before <= $2)
    OR (run.state = 'running' AND (run.lease_expires_at IS NULL OR run.lease_expires_at <= $2))
  )
  AND (
    SELECT count(*) FROM public.task_runs AS active
    WHERE active.account_uuid = run.account_uuid
      AND active.namespace_id = run.namespace_id
      AND active.state = 'running'
      AND active.lease_expires_at > $2
  ) < LEAST(namespace.max_active_runs, 2)
ORDER BY namespace.last_claimed_at NULLS FIRST, run.priority DESC, run.created_at, run.id
FOR UPDATE OF run SKIP LOCKED
LIMIT 1`, input.AccountID, input.Now))
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRun{}, ErrNoEligibleTask
	}
	if err != nil {
		return TaskRun{}, err
	}

	leaseToken := uuid.NewString()
	leaseHash := hashLeaseToken(leaseToken)
	leaseExpires := input.Now.Add(input.LeaseTTL)
	updated, err := tx.ExecContext(ctx, `UPDATE public.task_runs
SET state = 'running', attempt = attempt + 1, lease_owner = $1,
  lease_token_hash = $2, lease_expires_at = $3, fence = fence + 1, updated_at = $4
WHERE id = $5 AND account_uuid = $6`,
		input.WorkerID, leaseHash, leaseExpires, input.Now, run.ID, input.AccountID)
	if err != nil {
		return TaskRun{}, err
	}
	if err := requireOneRow(updated); err != nil {
		return TaskRun{}, ErrNoEligibleTask
	}
	if _, err := tx.ExecContext(ctx, `UPDATE public.task_namespaces
SET last_claimed_at = $1
WHERE id = $2 AND account_uuid = $3`, input.Now, run.NamespaceID, input.AccountID); err != nil {
		return TaskRun{}, err
	}
	run.State = TaskRunRunning
	run.Attempt++
	run.LeaseOwner = input.WorkerID
	run.LeaseToken = leaseToken
	run.LeaseExpires = leaseExpires
	run.Fence++
	run.UpdatedAt = input.Now

	locked, err := lockSession(ctx, tx, run.SessionID, run.AccountID)
	if err != nil {
		return TaskRun{}, err
	}
	payload := taskRunEventPayload(map[string]any{
		"leaseExpiresAt": leaseExpires.Format(time.RFC3339Nano),
	}, run.ID, run.Fence, TaskRunRunning)
	payloadJSON, err := marshalEventPayload(payload)
	if err != nil {
		return TaskRun{}, err
	}
	event := Event{
		SessionID: run.SessionID, Seq: locked.lastEventSeq + 1, Type: EventRunRunning,
		Payload: payload, ActorID: input.WorkerID, CreatedAt: input.Now,
	}
	if err := insertEvent(ctx, tx, event, payloadJSON); err != nil {
		return TaskRun{}, err
	}
	contextSummary := cloneMap(locked.context)
	contextSummary["lastRunState"] = TaskRunRunning
	if err := updateSessionAfterEvent(ctx, tx, run.AccountID, run.SessionID, event.Seq, 1, contextSummary, input.Now); err != nil {
		return TaskRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskRun{}, err
	}
	return run, nil
}

func (s *PostgresStore) RecordTaskRunEvent(ctx context.Context, input TaskRunEventInput) (TaskRunEventResult, error) {
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.TaskRunID = strings.TrimSpace(input.TaskRunID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	input.LeaseToken = strings.TrimSpace(input.LeaseToken)
	input.Type = strings.TrimSpace(input.Type)
	input.BridgeRef = strings.TrimSpace(input.BridgeRef)
	if input.AccountID == "" || input.TaskRunID == "" || input.ClientRequestID == "" || input.LeaseToken == "" || input.Type == "" {
		return TaskRunEventResult{}, ErrInvalidInput
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	nextState, terminal, err := stateForRunEvent(input.Type, TaskRunRunning)
	if err != nil {
		return TaskRunEventResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, leaseHash, err := loadTaskRunForUpdate(ctx, tx, input.TaskRunID, input.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRunEventResult{}, ErrNotFound
	}
	if err != nil {
		return TaskRunEventResult{}, err
	}
	locked, err := lockSession(ctx, tx, run.SessionID, input.AccountID)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	providedHash := hashLeaseToken(input.LeaseToken)
	if run.Fence != input.Fence || !run.LeaseExpires.After(input.CreatedAt) ||
		subtle.ConstantTimeCompare([]byte(leaseHash), []byte(providedHash)) != 1 {
		return TaskRunEventResult{}, ErrLeaseConflict
	}
	existing, err := findEventByRequest(ctx, tx, run.SessionID, input.ClientRequestID)
	if err == nil {
		if existingRunID, _ := existing.Payload["taskRunId"].(string); existingRunID != run.ID {
			return TaskRunEventResult{}, ErrAlreadyExists
		}
		if err := tx.Commit(); err != nil {
			return TaskRunEventResult{}, err
		}
		return TaskRunEventResult{
			Event: existing, TaskRun: run,
			SnapshotVer: locked.snapshotVersion, LastEventSeq: locked.lastEventSeq,
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TaskRunEventResult{}, err
	}
	if run.State != TaskRunRunning {
		return TaskRunEventResult{}, ErrLeaseConflict
	}

	payload := taskRunEventPayload(input.Payload, run.ID, input.Fence, nextState)
	payloadJSON, err := marshalEventPayload(payload)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	bridgeRef := run.BridgeRef
	if input.BridgeRef != "" {
		bridgeRef = input.BridgeRef
	}
	var update sql.Result
	if terminal {
		update, err = tx.ExecContext(ctx, `UPDATE public.task_runs
SET state = $1, bridge_task_ref = $2, updated_at = $3
WHERE id = $4 AND account_uuid = $5 AND fence = $6
  AND lease_token_hash = $7 AND lease_expires_at > $3 AND state = 'running'`,
			nextState, bridgeRef, input.CreatedAt, run.ID, input.AccountID, input.Fence, providedHash)
	} else {
		update, err = tx.ExecContext(ctx, `UPDATE public.task_runs
SET bridge_task_ref = $1, updated_at = $2
WHERE id = $3 AND account_uuid = $4 AND fence = $5
  AND lease_token_hash = $6 AND lease_expires_at > $2 AND state = 'running'`,
			bridgeRef, input.CreatedAt, run.ID, input.AccountID, input.Fence, providedHash)
	}
	if err != nil {
		return TaskRunEventResult{}, err
	}
	if err := requireOneRow(update); err != nil {
		return TaskRunEventResult{}, ErrLeaseConflict
	}
	event := Event{
		SessionID: run.SessionID, Seq: locked.lastEventSeq + 1, Type: input.Type,
		Payload: payload, ActorID: input.ActorID, ClientRequestID: input.ClientRequestID,
		CreatedAt: input.CreatedAt,
	}
	if err := insertEvent(ctx, tx, event, payloadJSON); err != nil {
		return TaskRunEventResult{}, err
	}
	contextSummary := cloneMap(locked.context)
	contextSummary["lastRunState"] = nextState
	if err := updateSessionAfterEvent(ctx, tx, run.AccountID, run.SessionID, event.Seq, 1, contextSummary, input.CreatedAt); err != nil {
		return TaskRunEventResult{}, err
	}
	run.State = nextState
	run.BridgeRef = bridgeRef
	run.UpdatedAt = input.CreatedAt
	if err := tx.Commit(); err != nil {
		return TaskRunEventResult{}, err
	}
	return TaskRunEventResult{
		Event: event, TaskRun: run,
		SnapshotVer: locked.snapshotVersion + 1, LastEventSeq: event.Seq,
	}, nil
}

func loadTaskRunForUpdate(ctx context.Context, tx *sql.Tx, taskRunID, accountID string) (TaskRun, string, error) {
	return scanTaskRun(tx.QueryRowContext(ctx, `SELECT id, account_uuid::text, namespace_id, session_id,
  client_request_id, state, priority, not_before, attempt, lease_owner, lease_token_hash,
  lease_expires_at, fence, bridge_task_ref, created_at, updated_at
FROM public.task_runs
WHERE id = $1 AND account_uuid = $2
FOR UPDATE`, taskRunID, accountID))
}

func updateSessionAfterEvent(
	ctx context.Context,
	tx *sql.Tx,
	accountID string,
	sessionID string,
	lastEventSeq int64,
	snapshotIncrement int64,
	contextSummary map[string]any,
	updatedAt time.Time,
) error {
	encodedContext, err := marshalContextSummary(contextSummary)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE public.task_sessions
SET last_event_seq = $1, snapshot_version = snapshot_version + $2,
  context_summary = $3, updated_at = $4
WHERE id = $5 AND account_uuid = $6`,
		lastEventSeq, snapshotIncrement, encodedContext, updatedAt, sessionID, accountID)
	if err != nil {
		return err
	}
	return requireOneRow(result)
}

func hashLeaseToken(token string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return fmt.Sprintf("%x", digest)
}

type lockedSessionState struct {
	namespaceID     string
	lastEventSeq    int64
	snapshotVersion int64
	context         map[string]any
	updatedAt       time.Time
}

func lockSession(ctx context.Context, tx *sql.Tx, sessionID, accountID string) (lockedSessionState, error) {
	var (
		state       lockedSessionState
		contextJSON []byte
	)
	err := tx.QueryRowContext(ctx, `SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at
FROM public.task_sessions
WHERE id = $1 AND account_uuid = $2
FOR UPDATE`, sessionID, accountID).Scan(
		&state.namespaceID, &state.lastEventSeq, &state.snapshotVersion, &contextJSON, &state.updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedSessionState{}, ErrNotFound
	}
	if err != nil {
		return lockedSessionState{}, err
	}
	state.context, err = decodeObject(contextJSON)
	if err != nil {
		return lockedSessionState{}, err
	}
	return state, nil
}

type scanner interface {
	Scan(...any) error
}

func scanSession(row scanner) (Session, error) {
	var (
		session     Session
		contextJSON []byte
	)
	if err := row.Scan(
		&session.ID, &session.AccountID, &session.NamespaceID, &session.Title,
		&session.SnapshotVer, &session.LastEventSeq, &session.LifecycleState,
		&contextJSON, &session.CreatedAt, &session.UpdatedAt,
	); err != nil {
		return Session{}, err
	}
	contextSummary, err := decodeObject(contextJSON)
	if err != nil {
		return Session{}, err
	}
	session.Context = contextSummary
	return session, nil
}

func findEventByRequest(ctx context.Context, tx *sql.Tx, sessionID, clientRequestID string) (Event, error) {
	row := tx.QueryRowContext(ctx, `SELECT seq, event_type, payload, actor_id, client_request_id, created_at
FROM public.task_session_events
WHERE session_id = $1 AND client_request_id = $2`, sessionID, clientRequestID)
	return scanEvent(row, sessionID)
}

func findEventBySequence(ctx context.Context, tx *sql.Tx, sessionID string, seq int64) (Event, error) {
	row := tx.QueryRowContext(ctx, `SELECT seq, event_type, payload, actor_id, client_request_id, created_at
FROM public.task_session_events
WHERE session_id = $1 AND seq = $2`, sessionID, seq)
	return scanEvent(row, sessionID)
}

func scanEvent(row scanner, sessionID string) (Event, error) {
	var (
		event       Event
		payloadJSON []byte
	)
	event.SessionID = sessionID
	if err := row.Scan(&event.Seq, &event.Type, &payloadJSON, &event.ActorID, &event.ClientRequestID, &event.CreatedAt); err != nil {
		return Event{}, err
	}
	payload, err := decodeObject(payloadJSON)
	if err != nil {
		return Event{}, err
	}
	event.Payload = payload
	return event, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, event Event, payloadJSON []byte) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO public.task_session_events
  (session_id, seq, event_type, payload, actor_id, client_request_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		event.SessionID, event.Seq, event.Type, payloadJSON, event.ActorID, event.ClientRequestID, event.CreatedAt)
	if err != nil {
		return mapPostgresError(err)
	}
	return requireOneRow(result)
}

func loadExistingMessageCommand(ctx context.Context, tx *sql.Tx, accountID string, locked lockedSessionState, message Event) (MessageCommandResult, error) {
	runID, _ := message.Payload["taskRunId"].(string)
	if strings.TrimSpace(runID) == "" || message.Type != EventMessageCreated {
		return MessageCommandResult{}, ErrAlreadyExists
	}
	run, _, err := scanTaskRun(tx.QueryRowContext(ctx, `SELECT id, account_uuid::text, namespace_id, session_id,
  client_request_id, state, priority, not_before, attempt, lease_owner, lease_token_hash,
  lease_expires_at, fence, bridge_task_ref, created_at, updated_at
FROM public.task_runs
WHERE id = $1 AND account_uuid = $2`, runID, accountID))
	if err != nil {
		return MessageCommandResult{}, err
	}
	queued, err := findEventBySequence(ctx, tx, message.SessionID, message.Seq+1)
	if err != nil {
		return MessageCommandResult{}, err
	}
	if queued.Type != EventRunQueued {
		return MessageCommandResult{}, ErrAlreadyExists
	}
	return MessageCommandResult{
		Message: message, Queued: queued, TaskRun: run,
		SnapshotVer: locked.snapshotVersion, LastEventSeq: locked.lastEventSeq,
	}, nil
}

func scanTaskRun(row scanner) (TaskRun, string, error) {
	var (
		run            TaskRun
		leaseTokenHash string
		leaseExpires   sql.NullTime
	)
	if err := row.Scan(
		&run.ID, &run.AccountID, &run.NamespaceID, &run.SessionID, &run.ClientRequestID,
		&run.State, &run.Priority, &run.NotBefore, &run.Attempt, &run.LeaseOwner,
		&leaseTokenHash, &leaseExpires, &run.Fence, &run.BridgeRef, &run.CreatedAt, &run.UpdatedAt,
	); err != nil {
		return TaskRun{}, "", err
	}
	if leaseExpires.Valid {
		run.LeaseExpires = leaseExpires.Time
	}
	return run, leaseTokenHash, nil
}

func marshalEventPayload(payload map[string]any) ([]byte, error) {
	if containsArtifactContent(payload) {
		return nil, ErrArtifactPayload
	}
	encoded, err := json.Marshal(cloneMap(payload))
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxEventPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	return encoded, nil
}

func marshalContextSummary(contextSummary map[string]any) ([]byte, error) {
	if containsArtifactContent(contextSummary) {
		return nil, ErrArtifactPayload
	}
	encoded, err := json.Marshal(cloneMap(contextSummary))
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxSnapshotBytes {
		return nil, ErrPayloadTooLarge
	}
	return encoded, nil
}

func appendMessageToContext(contextSummary map[string]any, message map[string]any) (map[string]any, []byte, error) {
	next := cloneMap(contextSummary)
	next["lastMessage"] = strings.TrimSpace(fmt.Sprint(message["text"]))
	messages, _ := next["messages"].([]any)
	messages = append(append([]any(nil), messages...), cloneMap(message))
	if len(messages) > maxSnapshotMessages {
		messages = messages[len(messages)-maxSnapshotMessages:]
	}
	for len(messages) > 0 {
		next["messages"] = messages
		encoded, err := marshalContextSummary(next)
		if err == nil {
			return next, encoded, nil
		}
		if !errors.Is(err, ErrPayloadTooLarge) {
			return nil, nil, err
		}
		messages = messages[1:]
	}
	return nil, nil, ErrPayloadTooLarge
}

func containsArtifactContent(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(key)))
			switch normalized {
			case "artifactbytes", "filebytes", "filecontent", "attachmentcontent", "base64", "blob", "toollog", "toollogs":
				return true
			}
			if containsArtifactContent(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsArtifactContent(child) {
				return true
			}
		}
	}
	return false
}

func decodeObject(encoded []byte) (map[string]any, error) {
	if len(encoded) == 0 {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return value, nil
}

func updateContextSummary(contextSummary, payload map[string]any) {
	if text, ok := payload["text"].(string); ok && strings.TrimSpace(text) != "" {
		contextSummary["lastMessage"] = text
	}
	if summary, ok := payload["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		contextSummary["summary"] = summary
	}
}

func requireOneRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func mapPostgresError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrAlreadyExists
		case "23503":
			return ErrAccountMismatch
		case "23514":
			return ErrPayloadTooLarge
		}
	}
	return fmt.Errorf("task session postgres: %w", err)
}
