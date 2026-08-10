package tasksession

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStoreCreatesPersonalNamespaceAndKeepsSessionsScoped(t *testing.T) {
	store := NewMemoryStore()

	personal, err := store.EnsurePersonalNamespace(context.Background(), "account-1", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("ensure personal namespace: %v", err)
	}
	if personal.Slug != "personal" || personal.AccountID != "account-1" {
		t.Fatalf("unexpected namespace: %+v", personal)
	}

	session, err := store.CreateSession(context.Background(), CreateSessionInput{
		ID:          "session-1",
		AccountID:   "account-1",
		NamespaceID: personal.ID,
		Title:       "Shared task",
		CreatedAt:   time.Unix(2, 0),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.ID != "session-1" || session.LastEventSeq != 0 {
		t.Fatalf("unexpected session: %+v", session)
	}

	if _, err := store.GetSession(context.Background(), "account-2", personal.ID, session.ID); err == nil {
		t.Fatal("expected account isolation error")
	}
}

func TestMemoryStoreAppendEventIsIdempotentAndUpdatesLightweightContext(t *testing.T) {
	store := NewMemoryStore()
	ns, err := store.CreateNamespace(context.Background(), CreateNamespaceInput{
		ID:        "ns-1",
		AccountID: "account-1",
		Slug:      "work",
		CreatedAt: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := store.CreateSession(context.Background(), CreateSessionInput{
		ID:          "session-1",
		AccountID:   "account-1",
		NamespaceID: ns.ID,
		CreatedAt:   time.Unix(2, 0),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	input := AppendEventInput{
		AccountID:       "account-1",
		SessionID:       "session-1",
		ClientRequestID: "client-1",
		Type:            EventMessageCreated,
		Payload:         map[string]any{"text": "hello"},
		CreatedAt:       time.Unix(3, 0),
	}
	first, err := store.AppendEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}
	second, err := store.AppendEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("append duplicate event: %v", err)
	}
	if first.Seq != 1 || second.Seq != first.Seq {
		t.Fatalf("duplicate append changed sequence: first=%+v second=%+v", first, second)
	}

	snapshot, err := store.GetSnapshot(context.Background(), "account-1", "session-1")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if snapshot.LastEventSeq != 1 || snapshot.Context["lastMessage"] != "hello" {
		t.Fatalf("unexpected lightweight snapshot: %+v", snapshot)
	}
}

func TestMemoryStoreClaimIsFairAcrossNamespacesAndRespectsLimits(t *testing.T) {
	store := NewMemoryStore()
	for _, input := range []CreateNamespaceInput{
		{ID: "ns-a", AccountID: "account-1", Slug: "a", MaxActiveRuns: 1},
		{ID: "ns-b", AccountID: "account-1", Slug: "b", MaxActiveRuns: 1},
	} {
		if _, err := store.CreateNamespace(context.Background(), input); err != nil {
			t.Fatalf("create namespace %s: %v", input.ID, err)
		}
	}
	for i, item := range []struct {
		id string
		ns string
	}{
		{"run-a-1", "ns-a"}, {"run-a-2", "ns-a"}, {"run-b-1", "ns-b"},
	} {
		if _, err := store.CreateSession(context.Background(), CreateSessionInput{
			ID: "session-" + item.id, AccountID: "account-1", NamespaceID: item.ns,
			CreatedAt: time.Unix(int64(i+1), 0),
		}); err != nil {
			t.Fatalf("create session %s: %v", item.id, err)
		}
		if _, err := store.EnqueueTaskRun(context.Background(), EnqueueTaskRunInput{
			ID: "session-" + item.id, AccountID: "account-1", NamespaceID: item.ns,
			SessionID: "session-" + item.id, Priority: i, NotBefore: time.Unix(1, 0), CreatedAt: time.Unix(int64(i+1), 0),
		}); err != nil {
			t.Fatalf("enqueue %s: %v", item.id, err)
		}
	}

	first, err := store.ClaimNext(context.Background(), ClaimInput{AccountID: "account-1", WorkerID: "bridge-1", Now: time.Unix(10, 0), MaxGlobalActive: 2, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	second, err := store.ClaimNext(context.Background(), ClaimInput{AccountID: "account-1", WorkerID: "bridge-1", Now: time.Unix(10, 0), MaxGlobalActive: 2, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatalf("claim second: %v", err)
	}
	if first.NamespaceID == second.NamespaceID {
		t.Fatalf("scheduler starved namespace: first=%+v second=%+v", first, second)
	}
	if _, err := store.ClaimNext(context.Background(), ClaimInput{AccountID: "account-1", WorkerID: "bridge-1", Now: time.Unix(10, 0), MaxGlobalActive: 2, LeaseTTL: time.Minute}); err != ErrNoEligibleTask {
		t.Fatalf("expected global limit, got %v", err)
	}
}
