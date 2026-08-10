package tasksession

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultNamespaceMaxActive = 2
	defaultGlobalMaxActive    = 5
	maxEventPayloadBytes      = 16 * 1024
)

// Store is the small control-plane contract shared by the API and scheduler.
// The first implementation is in-memory for TDD and local tests; the Postgres
// implementation can preserve the same transaction semantics behind it.
type Store interface {
	EnsurePersonalNamespace(context.Context, string, time.Time) (Namespace, error)
	CreateNamespace(context.Context, CreateNamespaceInput) (Namespace, error)
	CreateSession(context.Context, CreateSessionInput) (Session, error)
	GetSession(context.Context, string, string, string) (Session, error)
	AppendEvent(context.Context, AppendEventInput) (Event, error)
	GetSnapshot(context.Context, string, string) (Snapshot, error)
	EnqueueTaskRun(context.Context, EnqueueTaskRunInput) (TaskRun, error)
	ClaimNext(context.Context, ClaimInput) (TaskRun, error)
}

type MemoryStore struct {
	mu                 sync.Mutex
	namespaces         map[string]Namespace
	namespacesByAcct   map[string]map[string]string
	sessions           map[string]Session
	events             map[string][]Event
	eventByRequest     map[string]Event
	taskRuns           map[string]TaskRun
	lastClaimNamespace map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		namespaces:         make(map[string]Namespace),
		namespacesByAcct:   make(map[string]map[string]string),
		sessions:           make(map[string]Session),
		events:             make(map[string][]Event),
		eventByRequest:     make(map[string]Event),
		taskRuns:           make(map[string]TaskRun),
		lastClaimNamespace: make(map[string]string),
	}
}

func (s *MemoryStore) EnsurePersonalNamespace(_ context.Context, accountID string, now time.Time) (Namespace, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return Namespace{}, ErrAccountMismatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bySlug := s.namespacesByAcct[accountID]; bySlug != nil {
		if id := bySlug[NamespacePersonal]; id != "" {
			return cloneNamespace(s.namespaces[id]), nil
		}
	}
	return s.createNamespaceLocked(CreateNamespaceInput{
		ID:            uuid.NewString(),
		AccountID:     accountID,
		Slug:          NamespacePersonal,
		DisplayName:   "Personal",
		MaxActiveRuns: defaultNamespaceMaxActive,
		CreatedAt:     now,
	})
}

func (s *MemoryStore) CreateNamespace(_ context.Context, input CreateNamespaceInput) (Namespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createNamespaceLocked(input)
}

func (s *MemoryStore) createNamespaceLocked(input CreateNamespaceInput) (Namespace, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.Slug = strings.TrimSpace(input.Slug)
	if input.ID == "" || input.AccountID == "" || input.Slug == "" {
		return Namespace{}, ErrAccountMismatch
	}
	if _, exists := s.namespaces[input.ID]; exists {
		return Namespace{}, ErrAlreadyExists
	}
	bySlug := s.namespacesByAcct[input.AccountID]
	if bySlug == nil {
		bySlug = make(map[string]string)
		s.namespacesByAcct[input.AccountID] = bySlug
	}
	if _, exists := bySlug[input.Slug]; exists {
		return Namespace{}, ErrAlreadyExists
	}
	if input.MaxActiveRuns <= 0 {
		input.MaxActiveRuns = defaultNamespaceMaxActive
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	ns := Namespace{
		ID:            input.ID,
		AccountID:     input.AccountID,
		Slug:          input.Slug,
		DisplayName:   strings.TrimSpace(input.DisplayName),
		MaxActiveRuns: input.MaxActiveRuns,
		CreatedAt:     input.CreatedAt,
	}
	s.namespaces[ns.ID] = ns
	bySlug[ns.Slug] = ns.ID
	return cloneNamespace(ns), nil
}

func (s *MemoryStore) CreateSession(_ context.Context, input CreateSessionInput) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[input.ID]; exists {
		return Session{}, ErrAlreadyExists
	}
	ns, ok := s.namespaces[input.NamespaceID]
	if !ok || ns.AccountID != input.AccountID {
		return Session{}, ErrAccountMismatch
	}
	if strings.TrimSpace(input.ID) == "" {
		return Session{}, ErrNotFound
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	session := Session{
		ID:             input.ID,
		AccountID:      input.AccountID,
		NamespaceID:    input.NamespaceID,
		Title:          strings.TrimSpace(input.Title),
		LifecycleState: SessionReady,
		Context:        make(map[string]any),
		CreatedAt:      input.CreatedAt,
		UpdatedAt:      input.CreatedAt,
	}
	s.sessions[session.ID] = session
	return cloneSession(session), nil
}

func (s *MemoryStore) GetSession(_ context.Context, accountID, namespaceID, sessionID string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return Session{}, ErrNotFound
	}
	if session.AccountID != accountID || session.NamespaceID != namespaceID {
		return Session{}, ErrAccountMismatch
	}
	return cloneSession(session), nil
}

func (s *MemoryStore) AppendEvent(_ context.Context, input AppendEventInput) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[input.SessionID]
	if !ok {
		return Event{}, ErrNotFound
	}
	if session.AccountID != input.AccountID {
		return Event{}, ErrAccountMismatch
	}
	if input.ClientRequestID != "" {
		key := input.SessionID + "\x00" + input.ClientRequestID
		if event, exists := s.eventByRequest[key]; exists {
			return cloneEvent(event), nil
		}
	}
	payload, err := json.Marshal(input.Payload)
	if err != nil {
		return Event{}, err
	}
	if len(payload) > maxEventPayloadBytes {
		return Event{}, ErrPayloadTooLarge
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	event := Event{
		SessionID:       input.SessionID,
		Seq:             session.LastEventSeq + 1,
		Type:            strings.TrimSpace(input.Type),
		Payload:         cloneMap(input.Payload),
		ActorID:         strings.TrimSpace(input.ActorID),
		ClientRequestID: strings.TrimSpace(input.ClientRequestID),
		CreatedAt:       input.CreatedAt,
	}
	s.events[event.SessionID] = append(s.events[event.SessionID], event)
	if event.ClientRequestID != "" {
		s.eventByRequest[event.SessionID+"\x00"+event.ClientRequestID] = event
	}
	session.LastEventSeq = event.Seq
	session.SnapshotVer++
	session.UpdatedAt = event.CreatedAt
	if text, ok := input.Payload["text"].(string); ok && strings.TrimSpace(text) != "" {
		session.Context["lastMessage"] = text
	}
	if summary, ok := input.Payload["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		session.Context["summary"] = summary
	}
	s.sessions[session.ID] = session
	return cloneEvent(event), nil
}

func (s *MemoryStore) GetSnapshot(_ context.Context, accountID, sessionID string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	if session.AccountID != accountID {
		return Snapshot{}, ErrAccountMismatch
	}
	return Snapshot{
		SessionID:      session.ID,
		NamespaceID:    session.NamespaceID,
		Title:          session.Title,
		SnapshotVer:    session.SnapshotVer,
		LastEventSeq:   session.LastEventSeq,
		LifecycleState: session.LifecycleState,
		Context:        cloneMap(session.Context),
		UpdatedAt:      session.UpdatedAt,
	}, nil
}

func (s *MemoryStore) EnqueueTaskRun(_ context.Context, input EnqueueTaskRunInput) (TaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.taskRuns[input.ID]; exists {
		return TaskRun{}, ErrAlreadyExists
	}
	session, ok := s.sessions[input.SessionID]
	if !ok || session.AccountID != input.AccountID || session.NamespaceID != input.NamespaceID {
		return TaskRun{}, ErrAccountMismatch
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	if input.NotBefore.IsZero() {
		input.NotBefore = input.CreatedAt
	}
	run := TaskRun{
		ID:          input.ID,
		AccountID:   input.AccountID,
		NamespaceID: input.NamespaceID,
		SessionID:   input.SessionID,
		State:       TaskRunQueued,
		Priority:    input.Priority,
		NotBefore:   input.NotBefore,
		CreatedAt:   input.CreatedAt,
	}
	s.taskRuns[run.ID] = run
	return cloneTaskRun(run), nil
}

func (s *MemoryStore) ClaimNext(_ context.Context, input ClaimInput) (TaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	if input.MaxGlobalActive <= 0 {
		input.MaxGlobalActive = defaultGlobalMaxActive
	}
	if input.LeaseTTL <= 0 {
		input.LeaseTTL = time.Minute
	}
	if countActive(s.taskRuns, input.AccountID) >= input.MaxGlobalActive {
		return TaskRun{}, ErrNoEligibleTask
	}
	namespaceIDs := s.eligibleNamespaceIDsLocked(input)
	if len(namespaceIDs) == 0 {
		return TaskRun{}, ErrNoEligibleTask
	}
	last := s.lastClaimNamespace[input.AccountID]
	start := 0
	if last != "" {
		for i, id := range namespaceIDs {
			if id == last {
				start = (i + 1) % len(namespaceIDs)
				break
			}
		}
	}
	for offset := 0; offset < len(namespaceIDs); offset++ {
		namespaceID := namespaceIDs[(start+offset)%len(namespaceIDs)]
		ns := s.namespaces[namespaceID]
		if countActiveForNamespace(s.taskRuns, input.AccountID, namespaceID) >= ns.MaxActiveRuns {
			continue
		}
		candidate, ok := oldestEligibleRun(s.taskRuns, input.AccountID, namespaceID, input.Now)
		if !ok {
			continue
		}
		candidate.State = TaskRunRunning
		candidate.Attempt++
		candidate.LeaseOwner = input.WorkerID
		candidate.LeaseToken = uuid.NewString()
		candidate.LeaseExpires = input.Now.Add(input.LeaseTTL)
		candidate.Fence++
		s.taskRuns[candidate.ID] = candidate
		s.lastClaimNamespace[input.AccountID] = namespaceID
		return cloneTaskRun(candidate), nil
	}
	return TaskRun{}, ErrNoEligibleTask
}

func eligibleNamespaceIDs(tasks map[string]TaskRun, accountID string, namespaces map[string]Namespace, now time.Time) []string {
	seen := make(map[string]bool)
	for _, task := range tasks {
		if task.AccountID == accountID && task.State == TaskRunQueued && !task.NotBefore.After(now) {
			seen[task.NamespaceID] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		if _, ok := namespaces[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *MemoryStore) eligibleNamespaceIDsLocked(input ClaimInput) []string {
	return eligibleNamespaceIDs(s.taskRuns, input.AccountID, s.namespaces, input.Now)
}

func oldestEligibleRun(tasks map[string]TaskRun, accountID, namespaceID string, now time.Time) (TaskRun, bool) {
	var selected TaskRun
	found := false
	for _, task := range tasks {
		if task.AccountID != accountID || task.NamespaceID != namespaceID || task.State != TaskRunQueued || task.NotBefore.After(now) {
			continue
		}
		if !found || task.Priority > selected.Priority || (task.Priority == selected.Priority && task.CreatedAt.Before(selected.CreatedAt)) {
			selected = task
			found = true
		}
	}
	return selected, found
}

func countActive(tasks map[string]TaskRun, accountID string) int {
	count := 0
	for _, task := range tasks {
		if task.AccountID == accountID && task.State == TaskRunRunning {
			count++
		}
	}
	return count
}

func countActiveForNamespace(tasks map[string]TaskRun, accountID, namespaceID string) int {
	count := 0
	for _, task := range tasks {
		if task.AccountID == accountID && task.NamespaceID == namespaceID && task.State == TaskRunRunning {
			count++
		}
	}
	return count
}

func cloneNamespace(value Namespace) Namespace { return value }

func cloneSession(value Session) Session {
	value.Context = cloneMap(value.Context)
	return value
}

func cloneEvent(value Event) Event {
	value.Payload = cloneMap(value.Payload)
	return value
}

func cloneTaskRun(value TaskRun) TaskRun { return value }

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
