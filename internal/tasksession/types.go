package tasksession

import (
	"errors"
	"time"
)

const (
	NamespacePersonal = "personal"

	SessionReady    = "ready"
	TaskRunQueued   = "queued"
	TaskRunRunning  = "running"
	TaskRunComplete = "completed"
	TaskRunFailed   = "failed"
	TaskRunCanceled = "cancelled"

	EventMessageCreated          = "message.created"
	EventRunQueued               = "run.queued"
	EventRunRunning              = "run.running"
	EventRunProgressed           = "run.progressed"
	EventAssistantMessageCreated = "assistant.message.created"
	EventRunCompleted            = "run.completed"
	EventRunFailed               = "run.failed"
	EventRunCancelled            = "run.cancelled"
)

var (
	ErrAlreadyExists   = errors.New("task session already exists")
	ErrNotFound        = errors.New("task session not found")
	ErrAccountMismatch = errors.New("task session account mismatch")
	ErrPayloadTooLarge = errors.New("task session event payload is too large")
	ErrNoEligibleTask  = errors.New("no eligible task run")
	ErrInvalidInput    = errors.New("invalid task session input")
	ErrLeaseConflict   = errors.New("task run lease does not match")
	ErrArtifactPayload = errors.New("artifact content is not allowed in task session events")
)

type Namespace struct {
	ID            string
	AccountID     string
	Slug          string
	DisplayName   string
	MaxActiveRuns int
	CreatedAt     time.Time
}

type Session struct {
	ID             string
	AccountID      string
	NamespaceID    string
	Title          string
	SnapshotVer    int64
	LastEventSeq   int64
	LifecycleState string
	Context        map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Event struct {
	SessionID       string
	Seq             int64
	Type            string
	Payload         map[string]any
	ActorID         string
	ClientRequestID string
	CreatedAt       time.Time
}

type Snapshot struct {
	SessionID      string
	NamespaceID    string
	Title          string
	SnapshotVer    int64
	LastEventSeq   int64
	LifecycleState string
	Context        map[string]any
	TaskRun        *TaskRun
	UpdatedAt      time.Time
}

type TaskRun struct {
	ID              string
	AccountID       string
	NamespaceID     string
	SessionID       string
	ClientRequestID string
	State           string
	Priority        int
	NotBefore       time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Attempt         int
	LeaseOwner      string
	LeaseToken      string
	LeaseExpires    time.Time
	Fence           int64
	BridgeRef       string
}

type CreateNamespaceInput struct {
	ID            string
	AccountID     string
	Slug          string
	DisplayName   string
	MaxActiveRuns int
	CreatedAt     time.Time
}

type CreateSessionInput struct {
	ID          string
	AccountID   string
	NamespaceID string
	Title       string
	CreatedAt   time.Time
}

type AppendEventInput struct {
	AccountID       string
	SessionID       string
	ActorID         string
	ClientRequestID string
	Type            string
	Payload         map[string]any
	CreatedAt       time.Time
}

type EnqueueTaskRunInput struct {
	ID              string
	AccountID       string
	NamespaceID     string
	SessionID       string
	ClientRequestID string
	Priority        int
	NotBefore       time.Time
	CreatedAt       time.Time
}

// AppendMessageInput is the durable command boundary used by Bridge. The
// account identity must be derived from the authenticated principal before the
// command reaches this package; it is never accepted from a Web/Desktop body.
type AppendMessageInput struct {
	AccountID       string
	SessionID       string
	ActorID         string
	ClientRequestID string
	Text            string
	TaskRunID       string
	Priority        int
	NotBefore       time.Time
	CreatedAt       time.Time
}

// MessageCommandResult describes the two ordered events and the single task
// run created in one transaction. LastEventSeq is the replay cursor after both
// message.created and run.queued have been committed.
type MessageCommandResult struct {
	Message      Event
	Queued       Event
	TaskRun      TaskRun
	SnapshotVer  int64
	LastEventSeq int64
	Existing     bool
}

// TaskRunEventInput is a Bridge callback guarded by the active lease. The
// clear-text LeaseToken is accepted only for comparison and must never be
// persisted or logged.
type TaskRunEventInput struct {
	AccountID       string
	TaskRunID       string
	ActorID         string
	ClientRequestID string
	Fence           int64
	LeaseToken      string
	Type            string
	Payload         map[string]any
	BridgeRef       string
	CreatedAt       time.Time
}

type TaskRunEventResult struct {
	Event        Event
	TaskRun      TaskRun
	SnapshotVer  int64
	LastEventSeq int64
}

type ClaimInput struct {
	AccountID       string
	WorkerID        string
	Now             time.Time
	MaxGlobalActive int
	LeaseTTL        time.Duration
}
