package tasksession

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TASKSESSION_TEST_DATABASE_URL points at a disposable PostgreSQL server; each
// run creates and drops its own database there. Without it the test is skipped.
func newIntegrationStore(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	admin := strings.TrimSpace(os.Getenv("TASKSESSION_TEST_DATABASE_URL"))
	if admin == "" {
		t.Skip("TASKSESSION_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	adminDB, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	name := "tasksession_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})
	parsed, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE public.users (uuid UUID PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPostgresSchema(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	accountID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO public.users (uuid) VALUES ($1)`, accountID); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store, accountID
}

func newIntegrationSession(t *testing.T, store *PostgresStore, accountID string) Session {
	t.Helper()
	ctx := context.Background()
	namespace, err := store.EnsurePersonalNamespace(ctx, accountID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, CreateSessionInput{ID: uuid.NewString(), AccountID: accountID, NamespaceID: namespace.ID, Title: "it", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestPostgresSchedulerRoundTrip(t *testing.T) {
	store, accountID := newIntegrationStore(t)
	ctx := context.Background()
	session := newIntegrationSession(t, store, accountID)

	appended, err := store.AppendMessage(ctx, AppendMessageInput{
		AccountID: accountID, SessionID: session.ID, ClientRequestID: "msg-1", Text: "fix the failing test",
		Routing: map[string]any{"role": "engineer", "workspace": "demo-repo", "attachments": float64(2), "followUp": true},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := store.AppendMessage(ctx, AppendMessageInput{AccountID: accountID, SessionID: session.ID, ClientRequestID: "msg-bad", Text: "x", Routing: map[string]any{"shell": "x"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown routing key must be rejected, got %v", err)
	}

	now := time.Now().UTC()
	run, err := store.ClaimNext(ctx, ClaimInput{AccountID: accountID, WorkerID: "dsh-it", Now: now, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if run.ID != appended.TaskRun.ID || run.Fence != 1 || run.LeaseToken == "" {
		t.Fatalf("unexpected claim: %+v", run)
	}
	message, err := store.RunMessage(ctx, accountID, run.ID)
	if err != nil {
		t.Fatalf("run message: %v", err)
	}
	if message.Text != "fix the failing test" || message.Routing["role"] != "engineer" || message.Routing["attachments"] != float64(2) || message.Routing["followUp"] != true {
		t.Fatalf("unexpected message: %+v", message)
	}
	if _, err := store.RunMessage(ctx, uuid.NewString(), run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account must not read the message, got %v", err)
	}

	renew := RenewLeaseInput{AccountID: accountID, TaskRunID: run.ID, WorkerID: "dsh-it", Fence: run.Fence, LeaseToken: run.LeaseToken, LeaseTTL: 2 * time.Minute, Now: now.Add(10 * time.Second)}
	renewed, err := store.RenewLease(ctx, renew)
	if err != nil || renewed.CancelRequested || !renewed.TaskRun.LeaseExpires.Equal(now.Add(130*time.Second)) {
		t.Fatalf("renew: %+v %v", renewed, err)
	}
	for name, bad := range map[string]RenewLeaseInput{
		"fence":  {AccountID: accountID, TaskRunID: run.ID, WorkerID: "dsh-it", Fence: 0, LeaseToken: run.LeaseToken, LeaseTTL: time.Minute, Now: now},
		"token":  {AccountID: accountID, TaskRunID: run.ID, WorkerID: "dsh-it", Fence: run.Fence, LeaseToken: "wrong", LeaseTTL: time.Minute, Now: now},
		"worker": {AccountID: accountID, TaskRunID: run.ID, WorkerID: "other", Fence: run.Fence, LeaseToken: run.LeaseToken, LeaseTTL: time.Minute, Now: now},
	} {
		if _, err := store.RenewLease(ctx, bad); !errors.Is(err, ErrLeaseConflict) {
			t.Fatalf("%s: expected lease conflict, got %v", name, err)
		}
	}

	requested, err := store.RequestCancel(ctx, CancelRunInput{AccountID: accountID, SessionID: session.ID, TaskRunID: run.ID, ClientRequestID: "cancel-1"})
	if err != nil || requested.Event.Type != EventRunCancelRequested {
		t.Fatalf("cancel request: %+v %v", requested, err)
	}
	again, err := store.RequestCancel(ctx, CancelRunInput{AccountID: accountID, SessionID: session.ID, TaskRunID: run.ID, ClientRequestID: "cancel-2"})
	if err != nil || again.Event.Seq != requested.Event.Seq {
		t.Fatalf("second cancel must return the first request: %+v %v", again, err)
	}
	renew.Now = now.Add(20 * time.Second)
	renewed, err = store.RenewLease(ctx, renew)
	if err != nil || !renewed.CancelRequested {
		t.Fatalf("renew after cancel: %+v %v", renewed, err)
	}
	if _, err := store.RecordTaskRunEvent(ctx, TaskRunEventInput{
		AccountID: accountID, TaskRunID: run.ID, ActorID: "dsh-it", ClientRequestID: run.ID + ":1:9",
		Fence: run.Fence, LeaseToken: run.LeaseToken, Type: EventRunCancelled, Payload: map[string]any{"stopReason": "aborted"}, CreatedAt: now.Add(25 * time.Second),
	}); err != nil {
		t.Fatalf("cancelled event: %v", err)
	}
	renew.Now = now.Add(30 * time.Second)
	if _, err := store.RenewLease(ctx, renew); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("renew after settle: %v", err)
	}
	if _, err := store.RequestCancel(ctx, CancelRunInput{AccountID: accountID, SessionID: session.ID, TaskRunID: run.ID, ClientRequestID: "cancel-3"}); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("cancel after settle: %v", err)
	}
	events, err := store.ListEvents(ctx, accountID, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	if got := strings.Join(types, ","); got != "message.created,run.queued,run.running,run.cancel_requested,run.cancelled" {
		t.Fatalf("event order: %s", got)
	}
}

func TestPostgresSchedulerQueuedCancelAndExpiredLease(t *testing.T) {
	store, accountID := newIntegrationStore(t)
	ctx := context.Background()
	session := newIntegrationSession(t, store, accountID)
	queued, err := store.AppendMessage(ctx, AppendMessageInput{AccountID: accountID, SessionID: session.ID, ClientRequestID: "msg-q", Text: "later"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.RequestCancel(ctx, CancelRunInput{AccountID: accountID, SessionID: session.ID, TaskRunID: queued.TaskRun.ID, ClientRequestID: "cancel-q"})
	if err != nil || cancelled.Event.Type != EventRunCancelled || cancelled.TaskRun.State != TaskRunCanceled {
		t.Fatalf("queued cancel: %+v %v", cancelled, err)
	}
	if _, err := store.ClaimNext(ctx, ClaimInput{AccountID: accountID, WorkerID: "w", Now: time.Now().UTC(), LeaseTTL: time.Minute}); !errors.Is(err, ErrNoEligibleTask) {
		t.Fatalf("a cancelled run was claimed: %v", err)
	}

	if _, err := store.AppendMessage(ctx, AppendMessageInput{AccountID: accountID, SessionID: session.ID, ClientRequestID: "msg-e", Text: "crash"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := store.ClaimNext(ctx, ClaimInput{AccountID: accountID, WorkerID: "w1", Now: now, LeaseTTL: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	expired := now.Add(11 * time.Second)
	if _, err := store.RenewLease(ctx, RenewLeaseInput{AccountID: accountID, TaskRunID: first.ID, WorkerID: "w1", Fence: first.Fence, LeaseToken: first.LeaseToken, LeaseTTL: time.Minute, Now: expired}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("an expired lease must not renew: %v", err)
	}
	second, err := store.ClaimNext(ctx, ClaimInput{AccountID: accountID, WorkerID: "w2", Now: expired, LeaseTTL: time.Minute})
	if err != nil || second.ID != first.ID || second.Fence != first.Fence+1 {
		t.Fatalf("expired run must be reclaimed with a new fence: %+v %v", second, err)
	}
	if _, err := store.RecordTaskRunEvent(ctx, TaskRunEventInput{AccountID: accountID, TaskRunID: first.ID, ClientRequestID: "late", Fence: first.Fence, LeaseToken: first.LeaseToken, Type: EventRunCompleted, CreatedAt: expired}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("the fenced-out worker must not complete the run: %v", err)
	}
	if message, err := store.RunMessage(ctx, accountID, second.ID); err != nil || message.Text != "crash" || len(message.Routing) != 0 {
		t.Fatalf("reclaimed run message: %+v %v", message, err)
	}

}
