package tasksession

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type sha256HexArgument struct{}

func (sha256HexArgument) Match(value driver.Value) bool {
	text, ok := value.(string)
	return ok && len(text) == sha256.Size*2 && regexp.MustCompile(`^[a-f0-9]+$`).MatchString(text)
}

func TestPostgresSchemaKeepsArtifactsOutsideAccounts(t *testing.T) {
	schema := strings.ToLower(strings.Join(PostgresSchemaStatements(), "\n"))
	for _, forbidden := range []string{
		"create table public.artifacts",
		"create table if not exists public.artifacts",
		"artifact_bytes",
		"bytea",
		"base64",
	} {
		if strings.Contains(schema, forbidden) {
			t.Fatalf("task-session schema persists forbidden artifact data %q", forbidden)
		}
	}
	for _, required := range []string{
		"public.task_namespaces",
		"public.task_sessions",
		"public.task_session_events",
		"public.task_runs",
		"lease_token_hash",
		"bridge_task_ref",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("task-session schema missing %q", required)
		}
	}
}

func TestApplyPostgresSchemaIsOneDDLTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	for _, statement := range PostgresSchemaStatements() {
		mock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectCommit()
	if err := ApplyPostgresSchema(context.Background(), db); err != nil {
		t.Fatalf("apply postgres schema: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStoreAppendMessageCommitsEventsAndRunAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 11, 2, 3, 4, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at[\s\S]+FROM public.task_sessions[\s\S]+FOR UPDATE`).
		WithArgs("session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"namespace_id", "last_event_seq", "snapshot_version", "context_summary", "updated_at"}).
			AddRow("namespace-1", int64(4), int64(7), []byte(`{"summary":"before"}`), now.Add(-time.Minute)))
	mock.ExpectQuery(`SELECT seq, event_type, payload, actor_id, client_request_id, created_at[\s\S]+FROM public.task_session_events`).
		WithArgs("session-1", "client-1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO public.task_session_events`).
		WithArgs("session-1", int64(5), EventMessageCreated, sqlmock.AnyArg(), "11111111-1111-1111-1111-111111111111", "client-1", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO public.task_runs`).
		WithArgs("run-1", "11111111-1111-1111-1111-111111111111", "namespace-1", "session-1", "client-1", 3, now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO public.task_session_events`).
		WithArgs("session-1", int64(6), EventRunQueued, sqlmock.AnyArg(), "11111111-1111-1111-1111-111111111111", "", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE public.task_sessions`).
		WithArgs(int64(6), sqlmock.AnyArg(), now, "session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	result, err := store.AppendMessage(context.Background(), AppendMessageInput{
		AccountID:       "11111111-1111-1111-1111-111111111111",
		SessionID:       "session-1",
		ActorID:         "11111111-1111-1111-1111-111111111111",
		ClientRequestID: "client-1",
		Text:            "continue on desktop",
		TaskRunID:       "run-1",
		Priority:        3,
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
	if result.Message.Seq != 5 || result.Queued.Seq != 6 || result.TaskRun.ID != "run-1" {
		t.Fatalf("unexpected command result: %+v", result)
	}
	if result.SnapshotVer != 9 || result.LastEventSeq != 6 {
		t.Fatalf("unexpected cursor: %+v", result)
	}
	if result.TaskRun.NamespaceID != "namespace-1" || result.TaskRun.State != TaskRunQueued {
		t.Fatalf("unexpected task run: %+v", result.TaskRun)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStoreAppendMessageRetryReturnsOriginalEventAndRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 11, 2, 13, 14, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at[\s\S]+FROM public.task_sessions[\s\S]+FOR UPDATE`).
		WithArgs("session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"namespace_id", "last_event_seq", "snapshot_version", "context_summary", "updated_at"}).
			AddRow("namespace-1", int64(6), int64(9), []byte(`{"lastMessage":"first"}`), now))
	mock.ExpectQuery(`SELECT seq, event_type, payload, actor_id, client_request_id, created_at[\s\S]+FROM public.task_session_events`).
		WithArgs("session-1", "client-1").
		WillReturnRows(sqlmock.NewRows([]string{"seq", "event_type", "payload", "actor_id", "client_request_id", "created_at"}).
			AddRow(int64(5), EventMessageCreated, []byte(`{"schemaVersion":1,"text":"first","taskRunId":"run-original"}`),
				"11111111-1111-1111-1111-111111111111", "client-1", now))
	mock.ExpectQuery(`SELECT id, account_uuid::text[\s\S]+FROM public.task_runs`).
		WithArgs("run-original", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(taskRunRows().AddRow(
			"run-original", "11111111-1111-1111-1111-111111111111", "namespace-1", "session-1",
			"client-1", TaskRunQueued, 0, now, 0, "", "", nil, int64(0), "", now, now,
		))
	mock.ExpectQuery(`SELECT seq, event_type, payload, actor_id, client_request_id, created_at[\s\S]+FROM public.task_session_events`).
		WithArgs("session-1", int64(6)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "event_type", "payload", "actor_id", "client_request_id", "created_at"}).
			AddRow(int64(6), EventRunQueued, []byte(`{"schemaVersion":1,"taskRunId":"run-original","state":"queued"}`),
				"11111111-1111-1111-1111-111111111111", "", now))
	mock.ExpectCommit()

	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	result, err := store.AppendMessage(context.Background(), AppendMessageInput{
		AccountID: "11111111-1111-1111-1111-111111111111", SessionID: "session-1",
		ClientRequestID: "client-1", Text: "retry body is ignored", TaskRunID: "run-new", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("append duplicate message: %v", err)
	}
	if result.Message.Seq != 5 || result.Queued.Seq != 6 || result.TaskRun.ID != "run-original" {
		t.Fatalf("retry did not return original command: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStoreAppendMessageRollsBackWhenTaskRunInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 11, 2, 23, 24, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at[\s\S]+FROM public.task_sessions[\s\S]+FOR UPDATE`).
		WithArgs("session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"namespace_id", "last_event_seq", "snapshot_version", "context_summary", "updated_at"}).
			AddRow("namespace-1", int64(0), int64(0), []byte(`{}`), now))
	mock.ExpectQuery(`SELECT seq, event_type, payload, actor_id, client_request_id, created_at[\s\S]+FROM public.task_session_events`).
		WithArgs("session-1", "client-1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO public.task_session_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO public.task_runs`).
		WillReturnError(errors.New("postgres unavailable"))
	mock.ExpectRollback()

	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	_, err = store.AppendMessage(context.Background(), AppendMessageInput{
		AccountID: "11111111-1111-1111-1111-111111111111", SessionID: "session-1",
		ClientRequestID: "client-1", Text: "must rollback", TaskRunID: "run-1", CreatedAt: now,
	})
	if err == nil {
		t.Fatal("expected task-run insert failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMessagePayloadContainsOnlyLightweightReferences(t *testing.T) {
	payload, err := messageEventPayload("hello", "run-1")
	if err != nil {
		t.Fatalf("build message payload: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "artifact") {
		t.Fatalf("message payload unexpectedly contains artifact data: %s", encoded)
	}
}

func TestEventPayloadRejectsArtifactBytes(t *testing.T) {
	_, err := marshalEventPayload(map[string]any{
		"schemaVersion": 1,
		"result":        map[string]any{"artifactBytes": "AAECAw=="},
	})
	if !errors.Is(err, ErrArtifactPayload) {
		t.Fatalf("expected artifact payload rejection, got %v", err)
	}
	if _, err := marshalEventPayload(map[string]any{
		"schemaVersion": 1,
		"bridgeTaskRef": "opaque-task-reference",
	}); err != nil {
		t.Fatalf("opaque bridge reference must remain allowed: %v", err)
	}
}

func TestPostgresStoreClaimStoresLeaseHashAndReturnsOpaqueToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 11, 3, 4, 5, 0, time.UTC)
	expiresAt := now.Add(time.Minute)
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT count\(\*\)[\s\S]+FROM public.task_runs`).
		WithArgs("11111111-1111-1111-1111-111111111111", now).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT run.id[\s\S]+FOR UPDATE OF run SKIP LOCKED`).
		WithArgs("11111111-1111-1111-1111-111111111111", now).
		WillReturnRows(taskRunRows().AddRow(
			"run-1", "11111111-1111-1111-1111-111111111111", "namespace-1", "session-1",
			"client-1", TaskRunQueued, 3, now, 0, "", "", nil, int64(0), "", now, now,
		))
	mock.ExpectExec(`UPDATE public.task_runs[\s\S]+lease_token_hash`).
		WithArgs("bridge-1", sha256HexArgument{}, expiresAt, now, "run-1", "11111111-1111-1111-1111-111111111111").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE public.task_namespaces[\s\S]+last_claimed_at`).
		WithArgs(now, "namespace-1", "11111111-1111-1111-1111-111111111111").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at[\s\S]+FROM public.task_sessions[\s\S]+FOR UPDATE`).
		WithArgs("session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"namespace_id", "last_event_seq", "snapshot_version", "context_summary", "updated_at"}).
			AddRow("namespace-1", int64(2), int64(2), []byte(`{}`), now))
	mock.ExpectExec(`INSERT INTO public.task_session_events`).
		WithArgs("session-1", int64(3), EventRunRunning, sqlmock.AnyArg(), "bridge-1", "", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE public.task_sessions`).
		WithArgs(int64(3), int64(1), sqlmock.AnyArg(), now, "session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	run, err := store.ClaimNext(context.Background(), ClaimInput{
		AccountID: "11111111-1111-1111-1111-111111111111", WorkerID: "bridge-1",
		Now: now, MaxGlobalActive: 5, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim next: %v", err)
	}
	if run.LeaseToken == "" || run.LeaseExpires != expiresAt || run.Fence != 1 || run.State != TaskRunRunning {
		t.Fatalf("unexpected claimed run: %+v", run)
	}
	if len(run.LeaseToken) == sha256.Size*2 && regexp.MustCompile(`^[a-f0-9]+$`).MatchString(run.LeaseToken) {
		t.Fatalf("claim returned hash instead of opaque lease token: %q", run.LeaseToken)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStoreRejectsStaleTaskRunCallbackBeforeWritingEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 11, 4, 5, 6, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, account_uuid::text[\s\S]+FROM public.task_runs[\s\S]+FOR UPDATE`).
		WithArgs("run-1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(taskRunRows().AddRow(
			"run-1", "11111111-1111-1111-1111-111111111111", "namespace-1", "session-1",
			"client-1", TaskRunRunning, 3, now.Add(-time.Minute), 1, "bridge-1", hashLeaseToken("correct-token"),
			now.Add(time.Minute), int64(2), "", now.Add(-time.Minute), now.Add(-time.Minute),
		))
	mock.ExpectQuery(`SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at[\s\S]+FROM public.task_sessions[\s\S]+FOR UPDATE`).
		WithArgs("session-1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"namespace_id", "last_event_seq", "snapshot_version", "context_summary", "updated_at"}).
			AddRow("namespace-1", int64(3), int64(3), []byte(`{}`), now))
	mock.ExpectRollback()

	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	_, err = store.RecordTaskRunEvent(context.Background(), TaskRunEventInput{
		AccountID: "11111111-1111-1111-1111-111111111111", TaskRunID: "run-1",
		ActorID: "bridge-1", ClientRequestID: "callback-1", Fence: 1,
		LeaseToken: "stale-token", Type: EventRunCompleted,
		Payload: map[string]any{"schemaVersion": 1}, CreatedAt: now,
	})
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expected lease conflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStoreRecordsTerminalEventWithConditionalLeaseUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sql mock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 11, 5, 6, 7, 0, time.UTC)
	accountID := "11111111-1111-1111-1111-111111111111"
	leaseToken := "correct-token"
	leaseHash := hashLeaseToken(leaseToken)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, account_uuid::text[\s\S]+FROM public.task_runs[\s\S]+FOR UPDATE`).
		WithArgs("run-1", accountID).
		WillReturnRows(taskRunRows().AddRow(
			"run-1", accountID, "namespace-1", "session-1", "client-1", TaskRunRunning,
			3, now.Add(-time.Minute), 1, "bridge-1", leaseHash, now.Add(time.Minute), int64(2), "", now.Add(-time.Minute), now,
		))
	mock.ExpectQuery(`SELECT namespace_id, last_event_seq, snapshot_version, context_summary, updated_at[\s\S]+FROM public.task_sessions[\s\S]+FOR UPDATE`).
		WithArgs("session-1", accountID).
		WillReturnRows(sqlmock.NewRows([]string{"namespace_id", "last_event_seq", "snapshot_version", "context_summary", "updated_at"}).
			AddRow("namespace-1", int64(3), int64(3), []byte(`{}`), now))
	mock.ExpectQuery(`SELECT seq, event_type, payload, actor_id, client_request_id, created_at[\s\S]+FROM public.task_session_events`).
		WithArgs("session-1", "callback-1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`UPDATE public.task_runs[\s\S]+lease_token_hash = \$7[\s\S]+lease_expires_at > \$3`).
		WithArgs(TaskRunComplete, "bridge-task-1", now, "run-1", accountID, int64(2), leaseHash).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO public.task_session_events`).
		WithArgs("session-1", int64(4), EventRunCompleted, sqlmock.AnyArg(), "bridge-1", "callback-1", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE public.task_sessions`).
		WithArgs(int64(4), int64(1), sqlmock.AnyArg(), now, "session-1", accountID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	result, err := store.RecordTaskRunEvent(context.Background(), TaskRunEventInput{
		AccountID: accountID, TaskRunID: "run-1", ActorID: "bridge-1", ClientRequestID: "callback-1",
		Fence: 2, LeaseToken: leaseToken, Type: EventRunCompleted,
		Payload: map[string]any{"schemaVersion": 1, "summary": "done"}, BridgeRef: "bridge-task-1", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("record terminal event: %v", err)
	}
	if result.Event.Seq != 4 || result.TaskRun.State != TaskRunComplete || result.TaskRun.BridgeRef != "bridge-task-1" {
		t.Fatalf("unexpected callback result: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func taskRunRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_uuid", "namespace_id", "session_id", "client_request_id", "state",
		"priority", "not_before", "attempt", "lease_owner", "lease_token_hash",
		"lease_expires_at", "fence", "bridge_task_ref", "created_at", "updated_at",
	})
}

func TestHashLeaseTokenIsStableAndDoesNotEchoSecret(t *testing.T) {
	hash := hashLeaseToken("secret")
	if hash == "secret" || hash != fmt.Sprintf("%x", sha256.Sum256([]byte("secret"))) {
		t.Fatalf("unexpected lease hash %q", hash)
	}
}
