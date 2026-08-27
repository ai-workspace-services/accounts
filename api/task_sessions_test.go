package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
	"account/internal/tasksession"
)

func newTaskSessionHarness(t *testing.T) (*gin.Engine, string, string) {
	t.Helper()
	router, accountID, token, _, _ := newTaskSessionRuntimeHarness(t)
	return router, accountID, token
}

func newTaskSessionRuntimeHarness(t *testing.T) (*gin.Engine, string, string, store.Store, *auth.TokenService) {
	t.Helper()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Task Session User", Email: "task-session@example.com", EmailVerified: true, Active: true}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	tokenService := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: time.Hour,
		Store:         st,
	})
	pair, err := tokenService.GenerateTokenPair(user.ID, user.Email, nil)
	if err != nil {
		t.Fatalf("generate token pair: %v", err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false), WithTokenService(tokenService))
	return router, user.ID, pair.AccessToken, st, tokenService
}

func TestTaskSessionV1SnapshotReplayAndMessageIdempotency(t *testing.T) {
	router, _, token := newTaskSessionHarness(t)

	namespaces := performTaskSessionRequest(t, router, token, http.MethodGet, "/api/v1/namespaces", "")
	if namespaces.Code != http.StatusOK {
		t.Fatalf("namespace status = %d, body=%s", namespaces.Code, namespaces.Body.String())
	}
	if namespaces.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing no-store response policy: %q", namespaces.Header().Get("Cache-Control"))
	}
	var namespacePayload struct {
		Namespaces []struct {
			NamespaceID string `json:"namespaceId"`
			Slug        string `json:"slug"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(namespaces.Body.Bytes(), &namespacePayload); err != nil || len(namespacePayload.Namespaces) != 1 {
		t.Fatalf("decode namespaces: %v payload=%s", err, namespaces.Body.String())
	}
	namespaceID := namespacePayload.Namespaces[0].NamespaceID
	if namespacePayload.Namespaces[0].Slug != "personal" || namespaceID == "" {
		t.Fatalf("unexpected namespace: %+v", namespacePayload.Namespaces)
	}

	created := performTaskSessionRequest(t, router, token, http.MethodPost,
		"/api/v1/namespaces/"+namespaceID+"/sessions", `{"title":"Shared task"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	var snapshot taskSnapshotV1
	if err := json.Unmarshal(created.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.SessionID == "" || snapshot.NamespaceID != namespaceID || snapshot.LastEventSeq != 0 {
		t.Fatalf("unexpected created snapshot: %+v", snapshot)
	}

	messageBody := `{"clientRequestId":"request-1","text":"continue on web","run":{"priority":3}}`
	first := performTaskSessionRequest(t, router, token, http.MethodPost,
		"/api/v1/sessions/"+snapshot.SessionID+"/messages", messageBody)
	second := performTaskSessionRequest(t, router, token, http.MethodPost,
		"/api/v1/sessions/"+snapshot.SessionID+"/messages", messageBody)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("message statuses = %d/%d, bodies=%s/%s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstReceipt, secondReceipt appendTaskMessageV1Response
	if err := json.Unmarshal(first.Body.Bytes(), &firstReceipt); err != nil {
		t.Fatalf("decode first receipt: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondReceipt); err != nil {
		t.Fatalf("decode second receipt: %v", err)
	}
	if firstReceipt.Event.Seq != 1 || firstReceipt.Event.Type != tasksession.EventMessageCreated ||
		firstReceipt.TaskRun.ID == "" || firstReceipt.TaskRun.BridgeTaskRef != "" ||
		secondReceipt.Event.Seq != firstReceipt.Event.Seq || secondReceipt.TaskRun.ID != firstReceipt.TaskRun.ID {
		t.Fatalf("idempotent receipt mismatch: first=%+v second=%+v", firstReceipt, secondReceipt)
	}

	replay := performTaskSessionRequest(t, router, token, http.MethodGet,
		"/api/v1/sessions/"+snapshot.SessionID+"/events?after_seq=0&limit=100", "")
	var replayPayload struct {
		Events       []taskEventV1 `json:"events"`
		LastEventSeq int64         `json:"lastEventSeq"`
	}
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body=%s", replay.Code, replay.Body.String())
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayPayload); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if len(replayPayload.Events) != 2 || replayPayload.Events[0].Seq != 1 ||
		replayPayload.Events[1].Seq != 2 || replayPayload.LastEventSeq != 2 {
		t.Fatalf("unexpected ordered replay: %+v", replayPayload)
	}

	loaded := performTaskSessionRequest(t, router, token, http.MethodGet,
		"/api/v1/sessions/"+snapshot.SessionID, "")
	if err := json.Unmarshal(loaded.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode loaded snapshot: %v", err)
	}
	if snapshot.TaskRun == nil || snapshot.TaskRun.ID != firstReceipt.TaskRun.ID || snapshot.LastEventSeq != 2 {
		t.Fatalf("snapshot missing latest task run: %+v", snapshot)
	}

	listed := performTaskSessionRequest(t, router, token, http.MethodGet,
		"/api/v1/namespaces/"+namespaceID+"/sessions", "")
	var listedPayload struct {
		Sessions []taskSnapshotV1 `json:"sessions"`
	}
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", listed.Code, listed.Body.String())
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listedPayload); err != nil || len(listedPayload.Sessions) != 1 {
		t.Fatalf("decode session list: %v payload=%s", err, listed.Body.String())
	}
}

func TestTaskSessionV1RejectsLooseMessagePayloadsAndInvalidPagination(t *testing.T) {
	router, _, token := newTaskSessionHarness(t)
	created := performTaskSessionRequest(t, router, token, http.MethodPost,
		"/api/v1/namespaces/personal/sessions", `{"title":"strict"}`)
	var snapshot taskSnapshotV1
	if err := json.Unmarshal(created.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode created snapshot: %v", err)
	}
	for name, body := range map[string]string{
		"unknown field":      `{"clientRequestId":"request-1","text":"hello","artifact":"forbidden"}`,
		"invalid request id": `{"clientRequestId":"bad request id","text":"hello"}`,
		"trailing object":    `{"clientRequestId":"request-1","text":"hello"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := performTaskSessionRequest(t, router, token, http.MethodPost,
				"/api/v1/sessions/"+snapshot.SessionID+"/messages", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
		})
	}
	invalidLimit := performTaskSessionRequest(t, router, token, http.MethodGet,
		"/api/v1/namespaces/"+snapshot.NamespaceID+"/sessions?limit=501", "")
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, body=%s", invalidLimit.Code, invalidLimit.Body.String())
	}
}

func TestTaskSessionV1CrossAccountReadsReturnNotFoundEnvelope(t *testing.T) {
	router, _, ownerToken, st, tokenService := newTaskSessionRuntimeHarness(t)
	created := performTaskSessionRequest(t, router, ownerToken, http.MethodPost,
		"/api/v1/namespaces/personal/sessions", `{"title":"private"}`)
	var snapshot taskSnapshotV1
	if err := json.Unmarshal(created.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode created snapshot: %v", err)
	}

	other := &store.User{Name: "Other", Email: "other-task-session@example.com", EmailVerified: true, Active: true}
	if err := st.CreateUser(context.Background(), other); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	pair, err := tokenService.GenerateTokenPair(other.ID, other.Email, nil)
	if err != nil {
		t.Fatalf("create other token: %v", err)
	}
	response := performTaskSessionRequest(t, router, pair.AccessToken, http.MethodGet,
		"/api/v1/sessions/"+snapshot.SessionID, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-account status = %d, body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "task_session_not_found" {
		t.Fatalf("unexpected error envelope: %v payload=%s", err, response.Body.String())
	}
}

func performTaskSessionRequest(t *testing.T, router *gin.Engine, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestTaskSessionCreateAndReadUsesPersonalNamespace(t *testing.T) {
	router, accountID, token := newTaskSessionHarness(t)
	request := httptest.NewRequest(http.MethodPost, "/api/task-sessions", bytes.NewBufferString(`{"title":"Shared task"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", response.Code, response.Body.String())
	}
	var created taskSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.AccountID != accountID || created.NamespaceID == "" || created.SessionID == "" {
		t.Fatalf("unexpected create response: %+v", created)
	}
	read := httptest.NewRequest(http.MethodGet, "/api/task-sessions/"+created.SessionID, nil)
	read.Header.Set("Authorization", "Bearer "+token)
	readResponse := httptest.NewRecorder()
	router.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK {
		t.Fatalf("read status = %d, body=%s", readResponse.Code, readResponse.Body.String())
	}
}

func TestTaskSessionRequiresAuthentication(t *testing.T) {
	router, _, _ := newTaskSessionHarness(t)
	request := httptest.NewRequest(http.MethodPost, "/api/task-sessions", bytes.NewBufferString(`{"title":"unauthorized"}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestTaskSessionEventAppendIsIdempotent(t *testing.T) {
	router, _, token := newTaskSessionHarness(t)
	create := httptest.NewRequest(http.MethodPost, "/api/task-sessions", bytes.NewBufferString(`{"title":"events"}`))
	create.Header.Set("Authorization", "Bearer "+token)
	createdResponse := httptest.NewRecorder()
	router.ServeHTTP(createdResponse, create)
	var created taskSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	appendEvent := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/task-sessions/"+created.SessionID+"/events", bytes.NewBufferString(`{"type":"message.created","clientRequestId":"client-1","payload":{"text":"hello"}}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	first := appendEvent()
	second := appendEvent()
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("append statuses = %d/%d, bodies=%s/%s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstEvent, secondEvent struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstEvent); err != nil {
		t.Fatalf("decode first event: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondEvent); err != nil {
		t.Fatalf("decode second event: %v", err)
	}
	if firstEvent.Seq != 1 || secondEvent.Seq != 1 {
		t.Fatalf("expected idempotent seq 1, got %d/%d", firstEvent.Seq, secondEvent.Seq)
	}
}

func TestTaskSessionEventRejectsArtifactBytes(t *testing.T) {
	router, _, token := newTaskSessionHarness(t)
	create := httptest.NewRequest(http.MethodPost, "/api/task-sessions", bytes.NewBufferString(`{"title":"events"}`))
	create.Header.Set("Authorization", "Bearer "+token)
	createdResponse := httptest.NewRecorder()
	router.ServeHTTP(createdResponse, create)
	var created taskSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/task-sessions/"+created.SessionID+"/events",
		bytes.NewBufferString(`{"type":"run.completed","clientRequestId":"client-artifact","payload":{"artifactBytes":"AAECAw=="}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
}
