package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const testSchedulerToken = "scheduler-test-token"

func schedulerRequest(t *testing.T, router *gin.Engine, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/internal/task-scheduler"+path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func decodeBody(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
	return body
}

// newSchedulerSession creates a session for the harness user and returns its id.
func newSchedulerSession(t *testing.T, router *gin.Engine, token string) string {
	t.Helper()
	created := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/namespaces/personal/sessions", `{"title":"Scheduled"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", created.Code, created.Body.String())
	}
	return decodeBody(t, created)["sessionId"].(string)
}

func appendRoutedMessage(t *testing.T, router *gin.Engine, token, sessionID, clientRequestID, routing string) string {
	t.Helper()
	body := fmt.Sprintf(`{"clientRequestId":%q,"text":"fix the failing test","routing":%s}`, clientRequestID, routing)
	appended := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", body)
	if appended.Code != http.StatusCreated {
		t.Fatalf("append = %d %s", appended.Code, appended.Body.String())
	}
	return decodeBody(t, appended)["taskRun"].(map[string]any)["id"].(string)
}

func listEventTypes(t *testing.T, router *gin.Engine, token, sessionID string) []string {
	t.Helper()
	listed := performTaskSessionRequest(t, router, token, http.MethodGet, "/api/v1/sessions/"+sessionID+"/events", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list events = %d %s", listed.Code, listed.Body.String())
	}
	var payload struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(payload.Events))
	for _, event := range payload.Events {
		types = append(types, event.Type)
	}
	return types
}

func TestTaskSchedulerCredentialAndAccountAllowlist(t *testing.T) {
	router, accountID, _ := newTaskSessionHarness(t)
	claim := map[string]any{"accountId": accountID, "workerId": "dsh-mac-1", "leaseTtlSeconds": 60}

	t.Setenv(taskSchedulerTokenEnv, "")
	if response := schedulerRequest(t, router, testSchedulerToken, "/claim", claim); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured scheduler = %d", response.Code)
	}
	t.Setenv(taskSchedulerTokenEnv, testSchedulerToken)
	t.Setenv(taskSchedulerAccountsEnv, "someone-else")
	if response := schedulerRequest(t, router, "wrong", "/claim", claim); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", response.Code)
	}
	if response := schedulerRequest(t, router, "", "/claim", claim); response.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", response.Code)
	}
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-token")
	if response := schedulerRequest(t, router, "internal-token", "/claim", claim); response.Code != http.StatusUnauthorized {
		t.Fatalf("the general internal token must not open the scheduler: %d", response.Code)
	}
	if response := schedulerRequest(t, router, testSchedulerToken, "/claim", claim); response.Code != http.StatusForbidden {
		t.Fatalf("account outside the allowlist = %d", response.Code)
	}
	t.Setenv(taskSchedulerAccountsEnv, " other , "+accountID)
	if response := schedulerRequest(t, router, testSchedulerToken, "/claim", claim); response.Code != http.StatusNoContent {
		t.Fatalf("empty queue = %d %s", response.Code, response.Body.String())
	}
	for _, bad := range []map[string]any{
		{"accountId": accountID, "workerId": "", "leaseTtlSeconds": 60},
		{"accountId": accountID, "workerId": "has space", "leaseTtlSeconds": 60},
		{"accountId": accountID, "workerId": "w", "leaseTtlSeconds": 5},
		{"accountId": accountID, "workerId": "w", "leaseTtlSeconds": 3600},
		{"accountId": accountID, "workerId": "w", "leaseTtlSeconds": 60, "extra": true},
	} {
		if response := schedulerRequest(t, router, testSchedulerToken, "/claim", bad); response.Code != http.StatusBadRequest {
			t.Fatalf("bad claim %v = %d", bad, response.Code)
		}
	}
}

func TestTaskSchedulerClaimRenewCancelAndFencedEvents(t *testing.T) {
	router, accountID, token := newTaskSessionHarness(t)
	t.Setenv(taskSchedulerTokenEnv, testSchedulerToken)
	t.Setenv(taskSchedulerAccountsEnv, accountID)
	sessionID := newSchedulerSession(t, router, token)
	runID := appendRoutedMessage(t, router, token, sessionID, "msg-1", `{"role":"engineer","workspace":"demo-repo","attachments":2,"followUp":true}`)

	claimed := schedulerRequest(t, router, testSchedulerToken, "/claim", map[string]any{"accountId": accountID, "workerId": "dsh-mac-1", "leaseTtlSeconds": 60})
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim = %d %s", claimed.Code, claimed.Body.String())
	}
	body := decodeBody(t, claimed)
	run := body["run"].(map[string]any)
	message := body["message"].(map[string]any)
	if run["id"] != runID || run["sessionId"] != sessionID || run["leaseToken"] == "" || run["fence"].(float64) != 1 {
		t.Fatalf("unexpected run: %v", run)
	}
	routing := message["routing"].(map[string]any)
	if message["text"] != "fix the failing test" || routing["role"] != "engineer" || routing["workspace"] != "demo-repo" || routing["attachments"].(float64) != 2 || routing["followUp"] != true {
		t.Fatalf("unexpected message: %v", message)
	}
	if again := schedulerRequest(t, router, testSchedulerToken, "/claim", map[string]any{"accountId": accountID, "workerId": "dsh-mac-1", "leaseTtlSeconds": 60}); again.Code != http.StatusNoContent {
		t.Fatalf("second claim = %d", again.Code)
	}

	lease := map[string]any{"accountId": accountID, "workerId": "dsh-mac-1", "fence": run["fence"], "leaseToken": run["leaseToken"], "leaseTtlSeconds": 60}
	renewed := schedulerRequest(t, router, testSchedulerToken, "/runs/"+runID+"/renew", lease)
	if renewed.Code != http.StatusOK || decodeBody(t, renewed)["cancelRequested"] != false {
		t.Fatalf("renew = %d %s", renewed.Code, renewed.Body.String())
	}
	stale := map[string]any{"accountId": accountID, "workerId": "dsh-mac-1", "fence": 0, "leaseToken": run["leaseToken"], "leaseTtlSeconds": 60}
	if response := schedulerRequest(t, router, testSchedulerToken, "/runs/"+runID+"/renew", stale); response.Code != http.StatusConflict {
		t.Fatalf("stale fence renew = %d", response.Code)
	}
	otherWorker := map[string]any{"accountId": accountID, "workerId": "other", "fence": run["fence"], "leaseToken": run["leaseToken"], "leaseTtlSeconds": 60}
	if response := schedulerRequest(t, router, testSchedulerToken, "/runs/"+runID+"/renew", otherWorker); response.Code != http.StatusConflict {
		t.Fatalf("other worker renew = %d", response.Code)
	}

	event := func(clientRequestID, eventType string, payload map[string]any) *httptest.ResponseRecorder {
		return schedulerRequest(t, router, testSchedulerToken, "/runs/"+runID+"/events", map[string]any{
			"accountId": accountID, "workerId": "dsh-mac-1", "fence": run["fence"], "leaseToken": run["leaseToken"],
			"clientRequestId": clientRequestID, "type": eventType, "payload": payload,
		})
	}
	first := event(runID+":1:1", "run.progressed", map[string]any{"phase": "selected"})
	if first.Code != http.StatusOK {
		t.Fatalf("progress event = %d %s", first.Code, first.Body.String())
	}
	retry := event(runID+":1:1", "run.progressed", map[string]any{"phase": "selected"})
	if decodeBody(t, retry)["event"].(map[string]any)["seq"] != decodeBody(t, first)["event"].(map[string]any)["seq"] {
		t.Fatalf("retried append must return the original event: %s", retry.Body.String())
	}
	for _, forbidden := range []string{"message.created", "run.queued", "run.cancel_requested", "run.running"} {
		if response := event(runID+":x:"+forbidden, forbidden, map[string]any{}); response.Code != http.StatusBadRequest {
			t.Fatalf("worker appended %s: %d", forbidden, response.Code)
		}
	}
	if response := event(runID+"/1/2", "run.progressed", map[string]any{}); response.Code != http.StatusBadRequest {
		t.Fatalf("slash in clientRequestId accepted: %d", response.Code)
	}

	cancel := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/runs/"+runID+"/cancel", `{"clientRequestId":"cancel-1"}`)
	if cancel.Code != http.StatusOK || decodeBody(t, cancel)["event"].(map[string]any)["type"] != "run.cancel_requested" {
		t.Fatalf("cancel running = %d %s", cancel.Code, cancel.Body.String())
	}
	cancelAgain := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/runs/"+runID+"/cancel", `{"clientRequestId":"cancel-2"}`)
	if cancelAgain.Code != http.StatusOK || decodeBody(t, cancelAgain)["event"].(map[string]any)["seq"] != decodeBody(t, cancel)["event"].(map[string]any)["seq"] {
		t.Fatalf("a second cancel must not append another request: %s", cancelAgain.Body.String())
	}
	renewed = schedulerRequest(t, router, testSchedulerToken, "/runs/"+runID+"/renew", lease)
	if renewed.Code != http.StatusOK || decodeBody(t, renewed)["cancelRequested"] != true {
		t.Fatalf("renew after cancel = %d %s", renewed.Code, renewed.Body.String())
	}
	if done := event(runID+":1:9", "run.cancelled", map[string]any{"stopReason": "aborted"}); done.Code != http.StatusOK {
		t.Fatalf("cancelled event = %d %s", done.Code, done.Body.String())
	}
	if response := schedulerRequest(t, router, testSchedulerToken, "/runs/"+runID+"/renew", lease); response.Code != http.StatusConflict {
		t.Fatalf("renew after settle = %d", response.Code)
	}
	if late := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/runs/"+runID+"/cancel", `{"clientRequestId":"cancel-3"}`); late.Code != http.StatusConflict {
		t.Fatalf("cancel after settle = %d %s", late.Code, late.Body.String())
	}
	types := strings.Join(listEventTypes(t, router, token, sessionID), ",")
	if types != "message.created,run.queued,run.progressed,run.cancel_requested,run.cancelled" {
		t.Fatalf("unexpected event order: %s", types)
	}
}

func TestTaskSchedulerQueuedCancelNeverReachesAWorker(t *testing.T) {
	router, accountID, token := newTaskSessionHarness(t)
	t.Setenv(taskSchedulerTokenEnv, testSchedulerToken)
	t.Setenv(taskSchedulerAccountsEnv, accountID)
	sessionID := newSchedulerSession(t, router, token)
	runID := appendRoutedMessage(t, router, token, sessionID, "msg-q", `{"role":"engineer"}`)

	cancel := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/runs/"+runID+"/cancel", `{"clientRequestId":"cancel-q"}`)
	if cancel.Code != http.StatusOK || decodeBody(t, cancel)["taskRun"].(map[string]any)["state"] != "cancelled" {
		t.Fatalf("cancel queued = %d %s", cancel.Code, cancel.Body.String())
	}
	if claimed := schedulerRequest(t, router, testSchedulerToken, "/claim", map[string]any{"accountId": accountID, "workerId": "w", "leaseTtlSeconds": 60}); claimed.Code != http.StatusNoContent {
		t.Fatalf("cancelled run was claimed: %d %s", claimed.Code, claimed.Body.String())
	}
	if missing := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/runs/not-a-run/cancel", `{"clientRequestId":"cancel-x"}`); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown run cancel = %d", missing.Code)
	}
}

func TestTaskMessageRoutingIsValidated(t *testing.T) {
	router, _, token := newTaskSessionHarness(t)
	sessionID := newSchedulerSession(t, router, token)
	for _, routing := range []string{
		`{"role":"engineer","shell":"rm -rf /"}`,
		`{"workingDirectory":"/srv/ws"}`,
		`{"workspace":"../etc"}`,
		`{"workspace":"/srv/ws"}`,
		`{"projectId":"a/b"}`,
		`{"attachments":-1}`,
		`{"attachments":1.5}`,
		`{"followUp":"yes"}`,
		`{"role":7}`,
	} {
		body := fmt.Sprintf(`{"clientRequestId":"msg-bad","text":"x","routing":%s}`, routing)
		if response := performTaskSessionRequest(t, router, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", body); response.Code != http.StatusBadRequest {
			t.Fatalf("routing %s accepted: %d", routing, response.Code)
		}
	}
}
