package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/tasksession"
)

// The task scheduler routes let one orchestrator worker (DSH aiw-orchestrator)
// claim task runs, renew their lease and append fenced events. They take their
// own bearer credential, never the general internal service token, and serve
// only the accounts listed for that credential.
const (
	taskSchedulerTokenEnv    = "TASK_SCHEDULER_TOKEN"
	taskSchedulerAccountsEnv = "TASK_SCHEDULER_ACCOUNT_IDS"
	taskSchedulerAccountKey  = "taskSchedulerAccountID"
	maxWorkerIDLength        = 128
)

// Events a worker may append; message, queue and cancel-request events belong
// to the user's side of the session.
var schedulerEventTypes = map[string]bool{
	tasksession.EventRunProgressed:           true,
	tasksession.EventAssistantMessageCreated: true,
	tasksession.EventRunCompleted:            true,
	tasksession.EventRunFailed:               true,
	tasksession.EventRunCancelled:            true,
}

type schedulerClaimRequest struct {
	AccountID       string `json:"accountId"`
	WorkerID        string `json:"workerId"`
	LeaseTTLSeconds int    `json:"leaseTtlSeconds"`
}

type schedulerRenewRequest struct {
	AccountID       string `json:"accountId"`
	WorkerID        string `json:"workerId"`
	Fence           int64  `json:"fence"`
	LeaseToken      string `json:"leaseToken"`
	LeaseTTLSeconds int    `json:"leaseTtlSeconds"`
}

type schedulerEventRequest struct {
	AccountID       string         `json:"accountId"`
	WorkerID        string         `json:"workerId"`
	Fence           int64          `json:"fence"`
	LeaseToken      string         `json:"leaseToken"`
	ClientRequestID string         `json:"clientRequestId"`
	Type            string         `json:"type"`
	Payload         map[string]any `json:"payload"`
}

type schedulerRunV1 struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"sessionId"`
	NamespaceID    string    `json:"namespaceId"`
	Fence          int64     `json:"fence"`
	LeaseToken     string    `json:"leaseToken"`
	LeaseExpiresAt time.Time `json:"leaseExpiresAt"`
	Attempt        int       `json:"attempt"`
}

type schedulerMessageV1 struct {
	Text    string         `json:"text"`
	Routing map[string]any `json:"routing"`
}

func digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// taskSchedulerAuth checks the scheduler bearer token in constant time. The
// account allowlist is enforced per request once the body names its account.
func taskSchedulerAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := strings.TrimSpace(os.Getenv(taskSchedulerTokenEnv))
		if expected == "" {
			respondTaskSessionErrorEnvelope(c, http.StatusServiceUnavailable, "task_scheduler_unconfigured", "task scheduler is not configured")
			c.Abort()
			return
		}
		header := c.GetHeader("Authorization")
		presented, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || subtle.ConstantTimeCompare(digest(strings.TrimSpace(presented)), digest(expected)) != 1 {
			respondTaskSessionErrorEnvelope(c, http.StatusUnauthorized, "task_scheduler_unauthorized", "task scheduler credential is invalid")
			c.Abort()
			return
		}
		c.Next()
	}
}

func schedulerAccountAllowed(accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	for _, allowed := range strings.Split(os.Getenv(taskSchedulerAccountsEnv), ",") {
		if strings.TrimSpace(allowed) == accountID {
			return true
		}
	}
	return false
}

func validWorkerID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maxWorkerIDLength && validClientRequestID(value)
}

func schedulerLeaseTTL(seconds int) (time.Duration, bool) {
	ttl := time.Duration(seconds) * time.Second
	return ttl, ttl >= tasksession.MinSchedulerLeaseTTL && ttl <= tasksession.MaxSchedulerLeaseTTL
}

// schedulerAccount decodes the body and admits it only for an allowed account
// and a well-formed worker id.
func schedulerAccount(c *gin.Context, target any, accountID, workerID func() string) bool {
	if err := decodeTaskSessionJSON(c, target); err != nil {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_scheduler_request", "invalid task scheduler request")
		return false
	}
	if !schedulerAccountAllowed(accountID()) {
		respondTaskSessionErrorEnvelope(c, http.StatusForbidden, "task_scheduler_account_denied", "this scheduler may not serve that account")
		return false
	}
	if !validWorkerID(workerID()) {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_scheduler_worker", "workerId is required")
		return false
	}
	return true
}

func respondTaskSchedulerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tasksession.ErrLeaseConflict):
		respondTaskSessionErrorEnvelope(c, http.StatusConflict, "task_run_lease_conflict", "task run lease is not held")
	case errors.Is(err, tasksession.ErrRunFinished):
		respondTaskSessionErrorEnvelope(c, http.StatusConflict, "task_run_finished", "task run already finished")
	default:
		respondTaskSessionErrorV1(c, err)
	}
}

func (h *handler) claimTaskRun(c *gin.Context) {
	var request schedulerClaimRequest
	if !schedulerAccount(c, &request, func() string { return request.AccountID }, func() string { return request.WorkerID }) {
		return
	}
	ttl, ok := schedulerLeaseTTL(request.LeaseTTLSeconds)
	if !ok {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_scheduler_lease", "leaseTtlSeconds is out of range")
		return
	}
	accountID := strings.TrimSpace(request.AccountID)
	run, err := h.taskSessions.ClaimNext(c.Request.Context(), tasksession.ClaimInput{
		AccountID: accountID, WorkerID: strings.TrimSpace(request.WorkerID), Now: time.Now().UTC(), LeaseTTL: ttl,
	})
	if errors.Is(err, tasksession.ErrNoEligibleTask) {
		c.Status(http.StatusNoContent)
		return
	}
	if err != nil {
		respondTaskSchedulerError(c, err)
		return
	}
	message, err := h.taskSessions.RunMessage(c.Request.Context(), accountID, run.ID)
	if err != nil {
		// The run stays claimed until its lease expires, then returns to the queue.
		slog.Error("task scheduler claimed a run without its message", "taskRunId", run.ID, "error", err)
		respondTaskSchedulerError(c, err)
		return
	}
	if message.Routing == nil {
		message.Routing = map[string]any{}
	}
	c.JSON(http.StatusOK, gin.H{
		"run": schedulerRunV1{
			ID: run.ID, SessionID: run.SessionID, NamespaceID: run.NamespaceID, Fence: run.Fence,
			LeaseToken: run.LeaseToken, LeaseExpiresAt: run.LeaseExpires.UTC(), Attempt: run.Attempt,
		},
		"message": schedulerMessageV1{Text: message.Text, Routing: message.Routing},
	})
}

func (h *handler) renewTaskRunLease(c *gin.Context) {
	var request schedulerRenewRequest
	if !schedulerAccount(c, &request, func() string { return request.AccountID }, func() string { return request.WorkerID }) {
		return
	}
	ttl, ok := schedulerLeaseTTL(request.LeaseTTLSeconds)
	if !ok {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_scheduler_lease", "leaseTtlSeconds is out of range")
		return
	}
	result, err := h.taskSessions.RenewLease(c.Request.Context(), tasksession.RenewLeaseInput{
		AccountID: request.AccountID, TaskRunID: c.Param("runID"), WorkerID: request.WorkerID,
		Fence: request.Fence, LeaseToken: request.LeaseToken, LeaseTTL: ttl, Now: time.Now().UTC(),
	})
	if err != nil {
		respondTaskSchedulerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"leaseExpiresAt": result.TaskRun.LeaseExpires.UTC(), "cancelRequested": result.CancelRequested})
}

func (h *handler) appendTaskRunEvent(c *gin.Context) {
	var request schedulerEventRequest
	if !schedulerAccount(c, &request, func() string { return request.AccountID }, func() string { return request.WorkerID }) {
		return
	}
	if !schedulerEventTypes[strings.TrimSpace(request.Type)] || !validClientRequestID(request.ClientRequestID) {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_run_event", "event type or clientRequestId is not accepted")
		return
	}
	result, err := h.taskSessions.RecordTaskRunEvent(c.Request.Context(), tasksession.TaskRunEventInput{
		AccountID: request.AccountID, TaskRunID: c.Param("runID"), ActorID: strings.TrimSpace(request.WorkerID),
		ClientRequestID: request.ClientRequestID, Fence: request.Fence, LeaseToken: request.LeaseToken,
		Type: request.Type, Payload: request.Payload, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		respondTaskSchedulerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"event": eventV1(result.Event), "lastEventSeq": result.LastEventSeq})
}

type cancelTaskRunV1Request struct {
	ClientRequestID string `json:"clientRequestId"`
}

// cancelTaskRunV1 is the user's cancel: a queued run is cancelled at once; a
// running run records run.cancel_requested for its worker to act on.
func (h *handler) cancelTaskRunV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	var request cancelTaskRunV1Request
	if err := decodeTaskSessionJSON(c, &request); err != nil || !validClientRequestID(request.ClientRequestID) {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_cancel", "clientRequestId is required")
		return
	}
	result, err := h.taskSessions.RequestCancel(c.Request.Context(), tasksession.CancelRunInput{
		AccountID: accountID, SessionID: c.Param("sessionID"), TaskRunID: c.Param("runID"),
		ActorID: accountID, ClientRequestID: request.ClientRequestID, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		respondTaskSchedulerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"event": eventV1(result.Event), "taskRun": taskRunResponseV1(result.TaskRun), "lastEventSeq": result.LastEventSeq})
}
