package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/auth"
	"account/internal/tasksession"
)

type taskNamespaceV1 struct {
	NamespaceID   string    `json:"namespaceId"`
	Slug          string    `json:"slug"`
	DisplayName   string    `json:"displayName"`
	MaxActiveRuns int       `json:"maxActiveRuns"`
	CreatedAt     time.Time `json:"createdAt"`
}

type taskRunV1 struct {
	ID            string     `json:"id"`
	State         string     `json:"state"`
	BridgeTaskRef string     `json:"bridgeTaskRef"`
	Priority      int        `json:"priority"`
	NotBefore     *time.Time `json:"notBefore,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

type taskSnapshotV1 struct {
	SessionID       string         `json:"sessionId"`
	NamespaceID     string         `json:"namespaceId"`
	Title           string         `json:"title"`
	SnapshotVersion int64          `json:"snapshotVersion"`
	LastEventSeq    int64          `json:"lastEventSeq"`
	LifecycleState  string         `json:"lifecycleState"`
	Context         map[string]any `json:"context"`
	TaskRun         *taskRunV1     `json:"taskRun,omitempty"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

type taskEventV1 struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"createdAt"`
}

type createTaskSessionV1Request struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

type appendTaskMessageV1Request struct {
	ClientRequestID string `json:"clientRequestId"`
	Text            string `json:"text"`
	Run             *struct {
		Priority  int        `json:"priority"`
		NotBefore *time.Time `json:"notBefore"`
	} `json:"run,omitempty"`
}

type appendTaskMessageV1Response struct {
	SessionID       string      `json:"sessionId"`
	NamespaceID     string      `json:"namespaceId"`
	SnapshotVersion int64       `json:"snapshotVersion"`
	Event           taskEventV1 `json:"event"`
	TaskRun         taskRunV1   `json:"taskRun"`
}

func (h *handler) listTaskNamespacesV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	if _, err := h.taskSessions.EnsurePersonalNamespace(c.Request.Context(), accountID, time.Now().UTC()); err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	items, err := h.taskSessions.ListNamespaces(c.Request.Context(), accountID)
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	response := make([]taskNamespaceV1, 0, len(items))
	for _, item := range items {
		response = append(response, taskNamespaceV1{
			NamespaceID: item.ID, Slug: item.Slug, DisplayName: item.DisplayName,
			MaxActiveRuns: item.MaxActiveRuns, CreatedAt: item.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"namespaces": response})
}

func (h *handler) listTaskSessionsV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	namespaceID, err := h.resolveTaskNamespaceV1(c, accountID)
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	items, err := h.taskSessions.ListSessions(c.Request.Context(), accountID, namespaceID)
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	limit, err := parseBoundedInt64(c.Query("limit"), 100, 1, 500)
	if err != nil {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_session_limit", "limit must be between 1 and 500")
		return
	}
	cursor, err := parseBoundedInt64(c.Query("cursor"), 0, 0, int64(len(items)))
	if err != nil {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_session_cursor", "cursor must be a non-negative integer")
		return
	}
	start := int(cursor)
	end := start + int(limit)
	if end > len(items) {
		end = len(items)
	}
	response := make([]taskSnapshotV1, 0, end-start)
	for _, item := range items[start:end] {
		response = append(response, snapshotV1(item))
	}
	payload := gin.H{"sessions": response}
	if end < len(items) {
		payload["nextCursor"] = strconv.Itoa(end)
	}
	c.JSON(http.StatusOK, payload)
}

func (h *handler) createTaskSessionV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	var request createTaskSessionV1Request
	if err := decodeTaskSessionJSON(c, &request); err != nil {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_session", "invalid task session payload")
		return
	}
	namespaceID, err := h.resolveTaskNamespaceV1(c, accountID)
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	sessionID := strings.TrimSpace(request.SessionID)
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	session, err := h.taskSessions.CreateSession(c.Request.Context(), tasksession.CreateSessionInput{
		ID: sessionID, AccountID: accountID, NamespaceID: namespaceID,
		Title: request.Title, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	c.JSON(http.StatusCreated, snapshotV1(tasksession.Snapshot{
		SessionID: session.ID, NamespaceID: session.NamespaceID, Title: session.Title,
		SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq,
		LifecycleState: session.LifecycleState, Context: session.Context, UpdatedAt: session.UpdatedAt,
	}))
}

func (h *handler) getTaskSessionV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	snapshot, err := h.taskSessions.GetSnapshot(c.Request.Context(), accountID, c.Param("sessionID"))
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	c.JSON(http.StatusOK, snapshotV1(snapshot))
}

func (h *handler) listTaskSessionEventsV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	afterSeq, err := parseBoundedInt64(c.Query("after_seq"), 0, 0, int64(^uint64(0)>>1))
	if err != nil {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_event_cursor", "after_seq must be a non-negative integer")
		return
	}
	limit64, err := parseBoundedInt64(c.Query("limit"), 100, 1, 500)
	if err != nil {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_event_limit", "limit must be between 1 and 500")
		return
	}
	sessionID := c.Param("sessionID")
	events, err := h.taskSessions.ListEvents(c.Request.Context(), accountID, sessionID, afterSeq, int(limit64))
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	snapshot, err := h.taskSessions.GetSnapshot(c.Request.Context(), accountID, sessionID)
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	response := make([]taskEventV1, 0, len(events))
	for _, event := range events {
		response = append(response, eventV1(event))
	}
	c.JSON(http.StatusOK, gin.H{"events": response, "lastEventSeq": snapshot.LastEventSeq})
}

func (h *handler) appendTaskSessionMessageV1(c *gin.Context) {
	accountID, ok := taskSessionAccountID(c)
	if !ok {
		return
	}
	var request appendTaskMessageV1Request
	if err := decodeTaskSessionJSON(c, &request); err != nil || !validClientRequestID(request.ClientRequestID) || strings.TrimSpace(request.Text) == "" || len([]byte(request.Text)) > 64*1024 {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_message", "clientRequestId and text are required")
		return
	}
	priority := 0
	var notBefore time.Time
	if request.Run != nil {
		priority = request.Run.Priority
		if request.Run.NotBefore != nil {
			notBefore = request.Run.NotBefore.UTC()
		}
	}
	if priority < 0 {
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_priority", "priority must be non-negative")
		return
	}
	result, err := h.taskSessions.AppendMessage(c.Request.Context(), tasksession.AppendMessageInput{
		AccountID: accountID, SessionID: c.Param("sessionID"), ActorID: accountID,
		ClientRequestID: request.ClientRequestID, Text: request.Text,
		Priority: priority, NotBefore: notBefore, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		respondTaskSessionErrorV1(c, err)
		return
	}
	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	c.JSON(status, appendTaskMessageV1Response{
		SessionID: result.Message.SessionID, NamespaceID: result.TaskRun.NamespaceID,
		SnapshotVersion: result.SnapshotVer, Event: eventV1(result.Message), TaskRun: taskRunResponseV1(result.TaskRun),
	})
}

func (h *handler) resolveTaskNamespaceV1(c *gin.Context, accountID string) (string, error) {
	namespaceID := strings.TrimSpace(c.Param("namespaceID"))
	if namespaceID == tasksession.NamespacePersonal {
		namespace, err := h.taskSessions.EnsurePersonalNamespace(c.Request.Context(), accountID, time.Now().UTC())
		if err != nil {
			return "", err
		}
		return namespace.ID, nil
	}
	if namespaceID == "" {
		return "", tasksession.ErrInvalidInput
	}
	return namespaceID, nil
}

func taskSessionAccountID(c *gin.Context) (string, bool) {
	accountID := strings.TrimSpace(auth.GetUserID(c))
	if accountID == "" {
		respondTaskSessionErrorEnvelope(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return "", false
	}
	return accountID, true
}

func snapshotV1(snapshot tasksession.Snapshot) taskSnapshotV1 {
	response := taskSnapshotV1{
		SessionID: snapshot.SessionID, NamespaceID: snapshot.NamespaceID, Title: snapshot.Title,
		SnapshotVersion: snapshot.SnapshotVer, LastEventSeq: snapshot.LastEventSeq,
		LifecycleState: snapshot.LifecycleState, Context: snapshot.Context, UpdatedAt: snapshot.UpdatedAt,
	}
	if response.Context == nil {
		response.Context = map[string]any{}
	}
	if snapshot.TaskRun != nil {
		run := taskRunResponseV1(*snapshot.TaskRun)
		response.TaskRun = &run
	}
	return response
}

func taskRunResponseV1(run tasksession.TaskRun) taskRunV1 {
	response := taskRunV1{
		ID: run.ID, State: run.State, BridgeTaskRef: run.BridgeRef, Priority: run.Priority,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if !run.NotBefore.IsZero() {
		notBefore := run.NotBefore.UTC()
		response.NotBefore = &notBefore
	}
	return response
}

func eventV1(event tasksession.Event) taskEventV1 {
	return taskEventV1{Seq: event.Seq, Type: event.Type, Payload: event.Payload, CreatedAt: event.CreatedAt}
}

func parseBoundedInt64(raw string, fallback, minimum, maximum int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, tasksession.ErrInvalidInput
	}
	return value, nil
}

func decodeTaskSessionJSON(c *gin.Context, target any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return tasksession.ErrInvalidInput
		}
		return err
	}
	return nil
}

func validClientRequestID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return false
	}
	return true
}

func respondTaskSessionErrorV1(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tasksession.ErrNotFound), errors.Is(err, tasksession.ErrAccountMismatch):
		respondTaskSessionErrorEnvelope(c, http.StatusNotFound, "task_session_not_found", "task session was not found")
	case errors.Is(err, tasksession.ErrAlreadyExists):
		respondTaskSessionErrorEnvelope(c, http.StatusConflict, "task_session_exists", "task session already exists")
	case errors.Is(err, tasksession.ErrPayloadTooLarge):
		respondTaskSessionErrorEnvelope(c, http.StatusRequestEntityTooLarge, "task_session_payload_too_large", "task session payload is too large")
	case errors.Is(err, tasksession.ErrArtifactPayload), errors.Is(err, tasksession.ErrInvalidInput):
		respondTaskSessionErrorEnvelope(c, http.StatusBadRequest, "invalid_task_session_payload", "invalid task session payload")
	default:
		respondTaskSessionErrorEnvelope(c, http.StatusInternalServerError, "task_session_unavailable", "task session service is unavailable")
	}
}

func respondTaskSessionErrorEnvelope(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
