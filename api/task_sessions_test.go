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
)

func newTaskSessionHarness(t *testing.T) (*gin.Engine, string, string) {
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
	return router, user.ID, pair.AccessToken
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
