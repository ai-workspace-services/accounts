package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"account/internal/auth"
	"account/internal/tasksession"
)

type taskSessionResponse struct {
	AccountID      string         `json:"accountId"`
	NamespaceID    string         `json:"namespaceId"`
	SessionID      string         `json:"sessionId"`
	Title          string         `json:"title"`
	SnapshotVer    int64          `json:"snapshotVersion"`
	LastEventSeq   int64          `json:"lastEventSeq"`
	LifecycleState string         `json:"lifecycleState"`
	Context        map[string]any `json:"context,omitempty"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

type createTaskSessionRequest struct {
	SessionID   string `json:"sessionId"`
	NamespaceID string `json:"namespaceId"`
	Title       string `json:"title"`
}

type appendTaskSessionEventRequest struct {
	Type            string         `json:"type"`
	Payload         map[string]any `json:"payload"`
	ClientRequestID string         `json:"clientRequestId"`
}

func (h *handler) createTaskSession(c *gin.Context) {
	accountID := auth.GetUserID(c)
	if accountID == "" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return
	}
	var request createTaskSessionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_task_session", "invalid task session payload")
		return
	}
	now := time.Now().UTC()
	namespace, err := h.taskSessions.EnsurePersonalNamespace(c.Request.Context(), accountID, now)
	if err != nil {
		respondTaskSessionError(c, err)
		return
	}
	if strings.TrimSpace(request.NamespaceID) != "" && request.NamespaceID != namespace.ID && request.NamespaceID != tasksession.NamespacePersonal {
		respondError(c, http.StatusBadRequest, "unsupported_namespace", "only the personal namespace is available in the MVP")
		return
	}
	sessionID := strings.TrimSpace(request.SessionID)
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	session, err := h.taskSessions.CreateSession(c.Request.Context(), tasksession.CreateSessionInput{
		ID: sessionID, AccountID: accountID, NamespaceID: namespace.ID, Title: request.Title, CreatedAt: now,
	})
	if err != nil {
		respondTaskSessionError(c, err)
		return
	}
	c.JSON(http.StatusCreated, taskSessionResponseFromSession(session))
}

func (h *handler) getTaskSession(c *gin.Context) {
	accountID := auth.GetUserID(c)
	if accountID == "" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return
	}
	snapshot, err := h.taskSessions.GetSnapshot(c.Request.Context(), accountID, c.Param("sessionID"))
	if err != nil {
		respondTaskSessionError(c, err)
		return
	}
	c.JSON(http.StatusOK, taskSessionResponse{
		AccountID: accountID, NamespaceID: snapshot.NamespaceID, SessionID: snapshot.SessionID,
		Title: snapshot.Title, SnapshotVer: snapshot.SnapshotVer, LastEventSeq: snapshot.LastEventSeq,
		LifecycleState: snapshot.LifecycleState, Context: snapshot.Context, UpdatedAt: snapshot.UpdatedAt,
	})
}

func (h *handler) appendTaskSessionEvent(c *gin.Context) {
	accountID := auth.GetUserID(c)
	if accountID == "" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return
	}
	var request appendTaskSessionEventRequest
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Type) == "" {
		respondError(c, http.StatusBadRequest, "invalid_task_session_event", "event type and payload are required")
		return
	}
	event, err := h.taskSessions.AppendEvent(c.Request.Context(), tasksession.AppendEventInput{
		AccountID: accountID, SessionID: c.Param("sessionID"), ActorID: accountID,
		ClientRequestID: request.ClientRequestID, Type: request.Type, Payload: request.Payload,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		respondTaskSessionError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"sessionId": event.SessionID, "seq": event.Seq, "type": event.Type, "payload": event.Payload, "clientRequestId": event.ClientRequestID, "createdAt": event.CreatedAt})
}

func taskSessionResponseFromSession(session tasksession.Session) taskSessionResponse {
	return taskSessionResponse{
		AccountID: session.AccountID, NamespaceID: session.NamespaceID, SessionID: session.ID,
		Title: session.Title, SnapshotVer: session.SnapshotVer, LastEventSeq: session.LastEventSeq,
		LifecycleState: session.LifecycleState, Context: session.Context, UpdatedAt: session.UpdatedAt,
	}
}

func respondTaskSessionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tasksession.ErrNotFound):
		respondError(c, http.StatusNotFound, "task_session_not_found", err.Error())
	case errors.Is(err, tasksession.ErrAccountMismatch):
		respondError(c, http.StatusForbidden, "task_session_forbidden", "task session does not belong to this account")
	case errors.Is(err, tasksession.ErrAlreadyExists):
		respondError(c, http.StatusConflict, "task_session_exists", err.Error())
	case errors.Is(err, tasksession.ErrPayloadTooLarge):
		respondError(c, http.StatusRequestEntityTooLarge, "task_session_payload_too_large", err.Error())
	case errors.Is(err, tasksession.ErrArtifactPayload), errors.Is(err, tasksession.ErrInvalidInput):
		respondError(c, http.StatusBadRequest, "invalid_task_session_payload", err.Error())
	default:
		respondError(c, http.StatusInternalServerError, "task_session_unavailable", err.Error())
	}
}
