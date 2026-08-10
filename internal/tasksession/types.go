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

	EventMessageCreated = "message.created"
)

var (
	ErrAlreadyExists   = errors.New("task session already exists")
	ErrNotFound        = errors.New("task session not found")
	ErrAccountMismatch = errors.New("task session account mismatch")
	ErrPayloadTooLarge = errors.New("task session event payload is too large")
	ErrNoEligibleTask  = errors.New("no eligible task run")
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
	UpdatedAt      time.Time
}

type TaskRun struct {
	ID           string
	AccountID    string
	NamespaceID  string
	SessionID    string
	State        string
	Priority     int
	NotBefore    time.Time
	CreatedAt    time.Time
	Attempt      int
	LeaseOwner   string
	LeaseToken   string
	LeaseExpires time.Time
	Fence        int64
	BridgeRef    string
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
	ID          string
	AccountID   string
	NamespaceID string
	SessionID   string
	Priority    int
	NotBefore   time.Time
	CreatedAt   time.Time
}

type ClaimInput struct {
	AccountID       string
	WorkerID        string
	Now             time.Time
	MaxGlobalActive int
	LeaseTTL        time.Duration
}
