package tasksession

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

// EventRunCancelRequested records a user's cancel of a running run. The run
// stays running until its lease holder observes the request on renewal and
// appends run.cancelled; a queued run is cancelled outright instead.
const EventRunCancelRequested = "run.cancel_requested"

// ErrRunFinished rejects a command against a run that already settled.
var ErrRunFinished = errors.New("task run already finished")

const (
	routingStringMax     = 512
	routingRefMax        = 128
	routingAttachMax     = 1000
	MinSchedulerLeaseTTL = 10 * time.Second
	MaxSchedulerLeaseTTL = 10 * time.Minute
)

// routingStrings are the routing hints a client may attach to a message; the
// scheduler reads them as a request, and the orchestrator's policy decides.
// workspace is an opaque key the worker host maps to a directory it has
// authorized: Accounts never holds working-directory paths.
var routingStrings = map[string]int{
	"role": routingStringMax, "roleMode": routingStringMax, "roleModel": routingStringMax,
	"productMode": routingStringMax, "specialty": routingStringMax, "dataClass": routingStringMax,
	"projectId": routingRefMax, "workspace": routingRefMax,
}

var routingRefs = map[string]bool{"projectId": true, "workspace": true}

var routingBools = map[string]bool{"followUp": true, "previousTurnFailed": true}

// NormalizeRouting validates the routing hints of a message. Unknown keys,
// wrong types, oversized values and references that are not plain keys are
// rejected rather than dropped, so a client never believes a hint was kept.
func NormalizeRouting(input map[string]any) (map[string]any, error) {
	routing := make(map[string]any, len(input))
	for key, value := range input {
		switch {
		case routingStrings[key] > 0:
			text, ok := value.(string)
			if !ok || len(text) > routingStrings[key] {
				return nil, ErrInvalidInput
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if routingRefs[key] && !plainKey(text) {
				return nil, ErrInvalidInput
			}
			routing[key] = text
		case routingBools[key]:
			flag, ok := value.(bool)
			if !ok {
				return nil, ErrInvalidInput
			}
			routing[key] = flag
		case key == "attachments":
			count, ok := wholeNumber(value)
			if !ok || count < 0 || count > routingAttachMax {
				return nil, ErrInvalidInput
			}
			routing[key] = count
		default:
			return nil, ErrInvalidInput
		}
	}
	return routing, nil
}

// plainKey admits identifiers only: no separators, so no path can pass as one.
func plainKey(value string) bool {
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}

func wholeNumber(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case float64:
		if number != math.Trunc(number) || math.IsInf(number, 0) {
			return 0, false
		}
		return int(number), true
	default:
		return 0, false
	}
}

// RenewLeaseInput extends the lease of a running run for its current holder.
type RenewLeaseInput struct {
	AccountID  string
	TaskRunID  string
	WorkerID   string
	Fence      int64
	LeaseToken string
	LeaseTTL   time.Duration
	Now        time.Time
}

type RenewLeaseResult struct {
	TaskRun         TaskRun
	CancelRequested bool
}

// CancelRunInput is a user's cancel of one run in one of their sessions.
type CancelRunInput struct {
	AccountID       string
	SessionID       string
	TaskRunID       string
	ActorID         string
	ClientRequestID string
	CreatedAt       time.Time
}

// RunMessage is what a claimed run asks for: the user's message text and the
// routing hints sent with it.
type RunMessage struct {
	Text    string
	Routing map[string]any
}

// SchedulerStore is the scheduler-facing half of the task-session store.
type SchedulerStore interface {
	RenewLease(context.Context, RenewLeaseInput) (RenewLeaseResult, error)
	RequestCancel(context.Context, CancelRunInput) (TaskRunEventResult, error)
	RunMessage(ctx context.Context, accountID, taskRunID string) (RunMessage, error)
}

func normalizeRenew(input RenewLeaseInput) (RenewLeaseInput, error) {
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.TaskRunID = strings.TrimSpace(input.TaskRunID)
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	input.LeaseToken = strings.TrimSpace(input.LeaseToken)
	if input.AccountID == "" || input.TaskRunID == "" || input.WorkerID == "" || input.LeaseToken == "" {
		return input, ErrInvalidInput
	}
	if input.LeaseTTL < MinSchedulerLeaseTTL || input.LeaseTTL > MaxSchedulerLeaseTTL {
		return input, ErrInvalidInput
	}
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	return input, nil
}

func normalizeCancel(input CancelRunInput) (CancelRunInput, error) {
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.TaskRunID = strings.TrimSpace(input.TaskRunID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.ClientRequestID = strings.TrimSpace(input.ClientRequestID)
	if input.AccountID == "" || input.SessionID == "" || input.TaskRunID == "" || input.ClientRequestID == "" {
		return input, ErrInvalidInput
	}
	if input.ActorID == "" {
		input.ActorID = input.AccountID
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	return input, nil
}

func cancelRequestedPayload(taskRunID string) map[string]any {
	return map[string]any{"schemaVersion": 1, "taskRunId": taskRunID, "state": TaskRunRunning}
}

func cancelledBeforeStartPayload(taskRunID string) map[string]any {
	return map[string]any{"schemaVersion": 1, "taskRunId": taskRunID, "state": TaskRunCanceled, "reason": "cancelled_before_start"}
}

func runFinished(state string) bool {
	return state == TaskRunComplete || state == TaskRunFailed || state == TaskRunCanceled
}

func (s *MemoryStore) RenewLease(_ context.Context, input RenewLeaseInput) (RenewLeaseResult, error) {
	input, err := normalizeRenew(input)
	if err != nil {
		return RenewLeaseResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.taskRuns[input.TaskRunID]
	if !ok || run.AccountID != input.AccountID {
		return RenewLeaseResult{}, ErrNotFound
	}
	if run.State != TaskRunRunning || run.Fence != input.Fence || run.LeaseToken != input.LeaseToken ||
		run.LeaseOwner != input.WorkerID || !run.LeaseExpires.After(input.Now) {
		return RenewLeaseResult{}, ErrLeaseConflict
	}
	run.LeaseExpires = input.Now.Add(input.LeaseTTL)
	run.UpdatedAt = input.Now
	s.taskRuns[run.ID] = run
	return RenewLeaseResult{TaskRun: cloneTaskRun(run), CancelRequested: s.cancelRequestedLocked(run)}, nil
}

func (s *MemoryStore) cancelRequestedLocked(run TaskRun) bool {
	for _, event := range s.events[run.SessionID] {
		if event.Type == EventRunCancelRequested && event.Payload["taskRunId"] == run.ID {
			return true
		}
	}
	return false
}

func (s *MemoryStore) RequestCancel(_ context.Context, input CancelRunInput) (TaskRunEventResult, error) {
	input, err := normalizeCancel(input)
	if err != nil {
		return TaskRunEventResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[input.SessionID]
	if !ok || session.AccountID != input.AccountID {
		return TaskRunEventResult{}, ErrNotFound
	}
	run, ok := s.taskRuns[input.TaskRunID]
	if !ok || run.AccountID != input.AccountID || run.SessionID != input.SessionID {
		return TaskRunEventResult{}, ErrNotFound
	}
	requestKey := input.SessionID + "\x00" + input.ClientRequestID
	if existing, ok := s.eventByRequest[requestKey]; ok {
		if existing.Payload["taskRunId"] != run.ID {
			return TaskRunEventResult{}, ErrAlreadyExists
		}
		return TaskRunEventResult{Event: cloneEvent(existing), TaskRun: cloneTaskRun(run), SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq}, nil
	}
	var event Event
	switch {
	case runFinished(run.State):
		return TaskRunEventResult{}, ErrRunFinished
	case run.State == TaskRunQueued:
		run.State = TaskRunCanceled
		run.UpdatedAt = input.CreatedAt
		event = Event{Type: EventRunCancelled, Payload: cancelledBeforeStartPayload(run.ID)}
	case s.cancelRequestedLocked(run):
		for _, existing := range s.events[run.SessionID] {
			if existing.Type == EventRunCancelRequested && existing.Payload["taskRunId"] == run.ID {
				return TaskRunEventResult{Event: cloneEvent(existing), TaskRun: cloneTaskRun(run), SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq}, nil
			}
		}
	default:
		event = Event{Type: EventRunCancelRequested, Payload: cancelRequestedPayload(run.ID)}
	}
	event.SessionID = run.SessionID
	event.Seq = session.LastEventSeq + 1
	event.ActorID = input.ActorID
	event.ClientRequestID = input.ClientRequestID
	event.CreatedAt = input.CreatedAt
	s.events[run.SessionID] = append(s.events[run.SessionID], event)
	s.eventByRequest[requestKey] = event
	s.taskRuns[run.ID] = run
	session.LastEventSeq = event.Seq
	session.SnapshotVer++
	session.Context["lastRunState"] = run.State
	session.UpdatedAt = input.CreatedAt
	s.sessions[session.ID] = session
	return TaskRunEventResult{Event: cloneEvent(event), TaskRun: cloneTaskRun(run), SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq}, nil
}

func (s *MemoryStore) RunMessage(_ context.Context, accountID, taskRunID string) (RunMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.taskRuns[strings.TrimSpace(taskRunID)]
	if !ok || run.AccountID != strings.TrimSpace(accountID) {
		return RunMessage{}, ErrNotFound
	}
	for _, event := range s.events[run.SessionID] {
		if event.Type == EventMessageCreated && event.Payload["taskRunId"] == run.ID {
			text, _ := event.Payload["text"].(string)
			return RunMessage{Text: text, Routing: cloneMap(s.runRouting[run.ID])}, nil
		}
	}
	return RunMessage{}, ErrNotFound
}
