package tasksession

import (
	"context"
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
	maxSnapshotBytes          = 128 * 1024
	maxSnapshotMessages       = 100
)

// Store is the small control-plane contract shared by the API and scheduler.
// The first implementation is in-memory for TDD and local tests; the Postgres
// implementation can preserve the same transaction semantics behind it.
type Store interface {
	EnsurePersonalNamespace(context.Context, string, time.Time) (Namespace, error)
	CreateNamespace(context.Context, CreateNamespaceInput) (Namespace, error)
	ListNamespaces(context.Context, string) ([]Namespace, error)
	CreateSession(context.Context, CreateSessionInput) (Session, error)
	ListSessions(context.Context, string, string) ([]Snapshot, error)
	GetSession(context.Context, string, string, string) (Session, error)
	AppendEvent(context.Context, AppendEventInput) (Event, error)
	AppendMessage(context.Context, AppendMessageInput) (MessageCommandResult, error)
	GetSnapshot(context.Context, string, string) (Snapshot, error)
	ListEvents(context.Context, string, string, int64, int) ([]Event, error)
	EnqueueTaskRun(context.Context, EnqueueTaskRunInput) (TaskRun, error)
	ClaimNext(context.Context, ClaimInput) (TaskRun, error)
	RecordTaskRunEvent(context.Context, TaskRunEventInput) (TaskRunEventResult, error)
}

func (s *MemoryStore) ListNamespaces(_ context.Context, accountID string) ([]Namespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, ErrInvalidInput
	}
	items := make([]Namespace, 0)
	for _, namespace := range s.namespaces {
		if namespace.AccountID == accountID {
			items = append(items, cloneNamespace(namespace))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return items, nil
}

func (s *MemoryStore) ListSessions(_ context.Context, accountID, namespaceID string) ([]Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	namespaceID = strings.TrimSpace(namespaceID)
	namespace, ok := s.namespaces[namespaceID]
	if !ok || namespace.AccountID != accountID {
		return nil, ErrNotFound
	}
	items := make([]Snapshot, 0)
	for _, session := range s.sessions {
		if session.AccountID != accountID || session.NamespaceID != namespaceID {
			continue
		}
		snapshot := snapshotFromMemoryState(session, s.taskRuns)
		items = append(items, snapshot)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].SessionID < items[j].SessionID
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items, nil
}

func (s *MemoryStore) ListEvents(_ context.Context, accountID, sessionID string, afterSeq int64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return nil, ErrNotFound
	}
	if session.AccountID != strings.TrimSpace(accountID) {
		return nil, ErrAccountMismatch
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	result := make([]Event, 0, limit)
	for _, event := range s.events[session.ID] {
		if event.Seq <= afterSeq {
			continue
		}
		result = append(result, cloneEvent(event))
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (s *MemoryStore) AppendMessage(_ context.Context, input AppendMessageInput) (MessageCommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	input.AccountID = strings.TrimSpace(input.AccountID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	input.Text = strings.TrimSpace(input.Text)
	if input.AccountID == "" || input.SessionID == "" || input.ClientRequestID == "" || input.Text == "" {
		return MessageCommandResult{}, ErrInvalidInput
	}
	session, ok := s.sessions[input.SessionID]
	if !ok {
		return MessageCommandResult{}, ErrNotFound
	}
	if session.AccountID != input.AccountID {
		return MessageCommandResult{}, ErrAccountMismatch
	}
	requestKey := input.SessionID + "\x00" + input.ClientRequestID
	if existing, ok := s.eventByRequest[requestKey]; ok {
		runID, _ := existing.Payload["taskRunId"].(string)
		run, exists := s.taskRuns[runID]
		if !exists {
			return MessageCommandResult{}, ErrNotFound
		}
		queuedEvents := s.events[input.SessionID]
		for _, queued := range queuedEvents {
			if queued.Seq == existing.Seq+1 && queued.Type == EventRunQueued {
				return MessageCommandResult{
					Message: existing, Queued: cloneEvent(queued), TaskRun: cloneTaskRun(run),
					SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq, Existing: true,
				}, nil
			}
		}
		return MessageCommandResult{}, ErrNotFound
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	if input.NotBefore.IsZero() {
		input.NotBefore = input.CreatedAt
	}
	input.TaskRunID = strings.TrimSpace(input.TaskRunID)
	if input.TaskRunID == "" {
		input.TaskRunID = uuid.NewString()
	}
	if _, exists := s.taskRuns[input.TaskRunID]; exists {
		return MessageCommandResult{}, ErrAlreadyExists
	}
	messagePayload, err := messageEventPayload(input.Text, input.TaskRunID)
	if err != nil {
		return MessageCommandResult{}, err
	}
	queuedPayload := runQueuedEventPayload(input.TaskRunID)
	for _, payload := range []map[string]any{messagePayload, queuedPayload} {
		if _, err := marshalEventPayload(payload); err != nil {
			return MessageCommandResult{}, err
		}
	}

	message := Event{
		SessionID: input.SessionID, Seq: session.LastEventSeq + 1, Type: EventMessageCreated,
		Payload: messagePayload, ActorID: input.ActorID, ClientRequestID: input.ClientRequestID,
		CreatedAt: input.CreatedAt,
	}
	run := TaskRun{
		ID: input.TaskRunID, AccountID: input.AccountID, NamespaceID: session.NamespaceID,
		SessionID: input.SessionID, ClientRequestID: input.ClientRequestID, State: TaskRunQueued,
		Priority: input.Priority, NotBefore: input.NotBefore, CreatedAt: input.CreatedAt, UpdatedAt: input.CreatedAt,
	}
	queued := Event{
		SessionID: input.SessionID, Seq: message.Seq + 1, Type: EventRunQueued,
		Payload: queuedPayload, ActorID: input.ActorID, CreatedAt: input.CreatedAt,
	}
	nextContext, _, err := appendMessageToContext(session.Context, messageContextValue(input.Text, input.TaskRunID, input.CreatedAt))
	if err != nil {
		return MessageCommandResult{}, err
	}
	s.events[input.SessionID] = append(s.events[input.SessionID], message, queued)
	s.eventByRequest[requestKey] = message
	s.taskRuns[run.ID] = run
	session.LastEventSeq = queued.Seq
	session.SnapshotVer += 2
	session.Context = nextContext
	session.UpdatedAt = input.CreatedAt
	s.sessions[session.ID] = session
	return MessageCommandResult{
		Message: cloneEvent(message), Queued: cloneEvent(queued), TaskRun: cloneTaskRun(run),
		SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq,
	}, nil
}

func (s *MemoryStore) RecordTaskRunEvent(_ context.Context, input TaskRunEventInput) (TaskRunEventResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.TaskRunID = strings.TrimSpace(input.TaskRunID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	input.LeaseToken = strings.TrimSpace(input.LeaseToken)
	input.Type = strings.TrimSpace(input.Type)
	if input.AccountID == "" || input.TaskRunID == "" || input.ClientRequestID == "" || input.LeaseToken == "" {
		return TaskRunEventResult{}, ErrInvalidInput
	}
	run, ok := s.taskRuns[input.TaskRunID]
	if !ok || run.AccountID != input.AccountID {
		return TaskRunEventResult{}, ErrNotFound
	}
	session, ok := s.sessions[run.SessionID]
	if !ok {
		return TaskRunEventResult{}, ErrNotFound
	}
	requestKey := run.SessionID + "\x00" + input.ClientRequestID
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	if run.Fence != input.Fence || run.LeaseToken != input.LeaseToken || !run.LeaseExpires.After(input.CreatedAt) {
		return TaskRunEventResult{}, ErrLeaseConflict
	}
	if existing, ok := s.eventByRequest[requestKey]; ok {
		return TaskRunEventResult{
			Event: cloneEvent(existing), TaskRun: cloneTaskRun(run),
			SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq,
		}, nil
	}
	if run.State != TaskRunRunning {
		return TaskRunEventResult{}, ErrLeaseConflict
	}
	nextState, _, err := stateForRunEvent(input.Type, run.State)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	payload := taskRunEventPayload(input.Payload, run.ID, input.Fence, nextState)
	if _, err := marshalEventPayload(payload); err != nil {
		return TaskRunEventResult{}, err
	}
	event := Event{
		SessionID: run.SessionID, Seq: session.LastEventSeq + 1, Type: input.Type,
		Payload: payload, ActorID: strings.TrimSpace(input.ActorID), ClientRequestID: input.ClientRequestID,
		CreatedAt: input.CreatedAt,
	}
	run.State = nextState
	if bridgeRef := strings.TrimSpace(input.BridgeRef); bridgeRef != "" {
		run.BridgeRef = bridgeRef
	}
	run.UpdatedAt = input.CreatedAt
	s.events[run.SessionID] = append(s.events[run.SessionID], event)
	s.eventByRequest[requestKey] = event
	s.taskRuns[run.ID] = run
	session.LastEventSeq = event.Seq
	session.SnapshotVer++
	session.Context["lastRunState"] = nextState
	session.UpdatedAt = input.CreatedAt
	s.sessions[session.ID] = session
	return TaskRunEventResult{
		Event: cloneEvent(event), TaskRun: cloneTaskRun(run),
		SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq,
	}, nil
}

func stateForRunEvent(eventType, currentState string) (string, bool, error) {
	switch strings.TrimSpace(eventType) {
	case EventRunRunning, EventRunProgressed, EventAssistantMessageCreated:
		return currentState, false, nil
	case EventRunCompleted:
		return TaskRunComplete, true, nil
	case EventRunFailed:
		return TaskRunFailed, true, nil
	case EventRunCancelled:
		return TaskRunCanceled, true, nil
	default:
		return "", false, ErrInvalidInput
	}
}

func taskRunEventPayload(input map[string]any, taskRunID string, fence int64, state string) map[string]any {
	payload := cloneMap(input)
	payload["schemaVersion"] = 1
	payload["taskRunId"] = strings.TrimSpace(taskRunID)
	payload["fence"] = fence
	payload["state"] = state
	return payload
}

func messageEventPayload(text, taskRunID string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	taskRunID = strings.TrimSpace(taskRunID)
	if text == "" || taskRunID == "" {
		return nil, ErrInvalidInput
	}
	return map[string]any{
		"schemaVersion": 1,
		"text":          text,
		"taskRunId":     taskRunID,
	}, nil
}

func runQueuedEventPayload(taskRunID string) map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"taskRunId":     strings.TrimSpace(taskRunID),
		"state":         TaskRunQueued,
	}
}

func messageContextValue(text, taskRunID string, createdAt time.Time) map[string]any {
	return map[string]any{
		"id":          strings.TrimSpace(taskRunID),
		"role":        "user",
		"text":        strings.TrimSpace(text),
		"timestampMs": createdAt.UnixMilli(),
		"pending":     false,
		"error":       false,
	}
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
	if input.MaxActiveRuns <= 0 || input.MaxActiveRuns > defaultNamespaceMaxActive {
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
	if _, err := marshalEventPayload(input.Payload); err != nil {
		return Event{}, err
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
	return snapshotFromMemoryState(session, s.taskRuns), nil
}

func snapshotFromMemoryState(session Session, runs map[string]TaskRun) Snapshot {
	snapshot := Snapshot{
		SessionID:      session.ID,
		NamespaceID:    session.NamespaceID,
		Title:          session.Title,
		SnapshotVer:    session.SnapshotVer,
		LastEventSeq:   session.LastEventSeq,
		LifecycleState: session.LifecycleState,
		Context:        cloneMap(session.Context),
		UpdatedAt:      session.UpdatedAt,
	}
	for _, run := range runs {
		if run.SessionID != session.ID {
			continue
		}
		if snapshot.TaskRun == nil || run.UpdatedAt.After(snapshot.TaskRun.UpdatedAt) ||
			(run.UpdatedAt.Equal(snapshot.TaskRun.UpdatedAt) && run.ID > snapshot.TaskRun.ID) {
			copy := cloneTaskRun(run)
			snapshot.TaskRun = &copy
		}
	}
	return snapshot
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
		ID:              input.ID,
		AccountID:       input.AccountID,
		NamespaceID:     input.NamespaceID,
		SessionID:       input.SessionID,
		ClientRequestID: input.ClientRequestID,
		State:           TaskRunQueued,
		Priority:        input.Priority,
		NotBefore:       input.NotBefore,
		CreatedAt:       input.CreatedAt,
		UpdatedAt:       input.CreatedAt,
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
	if input.MaxGlobalActive <= 0 || input.MaxGlobalActive > defaultGlobalMaxActive {
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
		candidate.UpdatedAt = input.Now
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
