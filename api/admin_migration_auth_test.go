package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMigrationInternalServiceAuthorization(t *testing.T) {
	h := newActiveHarness(t)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "test-service-token")
	call := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/migration/authorize", bytes.NewBufferString(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		return w
	}
	for _, scope := range []string{"source.read", "target.plan", "target.apply"} {
		w := call("test-service-token", `{"scope":"`+scope+`"}`)
		var result map[string]string
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result["operator_id"]) != 64 || result["scope"] != scope || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "test-service-token") {
			t.Fatalf("invalid authorization for %s: %d", scope, w.Code)
		}
	}
	for _, token := range []string{"", "invalid", h.adminToken} {
		if call(token, `{"scope":"source.read"}`).Code != http.StatusUnauthorized {
			t.Fatal("invalid token accepted")
		}
	}
	if call("test-service-token", `{"scope":"unknown"}`).Code != http.StatusBadRequest {
		t.Fatal("unknown scope accepted")
	}
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	if call("test-service-token", `{"scope":"source.read"}`).Code != http.StatusUnauthorized {
		t.Fatal("unset credential accepted")
	}
}
