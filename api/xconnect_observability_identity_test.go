package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

func TestResolveXConnectTelemetryIdentityByEmailOrProxyUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("XCONNECT_OBSERVABILITY_SEARCH_TOKEN", "observability-test-token-32-bytes!!")
	st := store.NewMemoryStore()
	user := &store.User{
		Name: "Diagnostics User", Email: "diagnostics@example.test", PasswordHash: "unused",
		EmailVerified: true, Active: true, ProxyUUID: "a55a9f3a-5d63-4b99-9c3a-9dfeb6d57da2",
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	resolve := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/observability/internal/xconnect/identity", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-XConnect-Observability-Token", os.Getenv("XCONNECT_OBSERVABILITY_SEARCH_TOKEN"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	emailResponse := resolve(`{"email":"DIAGNOSTICS@example.test"}`)
	proxyResponse := resolve(`{"proxyUuid":"a55a9f3a-5d63-4b99-9c3a-9dfeb6d57da2"}`)
	if emailResponse.Code != http.StatusOK || proxyResponse.Code != http.StatusOK {
		t.Fatalf("lookup statuses: email=%d uuid=%d", emailResponse.Code, proxyResponse.Code)
	}
	if emailResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("identity response is cacheable: %q", emailResponse.Header().Get("Cache-Control"))
	}
	var emailPayload, proxyPayload struct {
		Found            bool   `json:"found"`
		ProxyIdentityRef string `json:"proxyIdentityRef"`
	}
	if err := json.Unmarshal(emailResponse.Body.Bytes(), &emailPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(proxyResponse.Body.Bytes(), &proxyPayload); err != nil {
		t.Fatal(err)
	}
	if !emailPayload.Found || !proxyPayload.Found || emailPayload.ProxyIdentityRef == "" || emailPayload.ProxyIdentityRef != proxyPayload.ProxyIdentityRef {
		t.Fatalf("email and UUID did not resolve to the same opaque identity: email=%+v uuid=%+v", emailPayload, proxyPayload)
	}
	for _, raw := range []string{user.Email, user.ProxyUUID} {
		if bytes.Contains(emailResponse.Body.Bytes(), []byte(raw)) || bytes.Contains(proxyResponse.Body.Bytes(), []byte(raw)) {
			t.Fatalf("raw identity %q leaked in resolver response", raw)
		}
	}
}

func TestResolveXConnectTelemetryIdentityRequiresExactlyOneIdentifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("XCONNECT_OBSERVABILITY_SEARCH_TOKEN", "observability-test-token-32-bytes!!")
	router := gin.New()
	RegisterRoutes(router, WithStore(store.NewMemoryStore()), WithEmailVerification(false))
	for _, body := range []string{`{}`, `{"email":"a@example.test","proxyUuid":"credential"}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/observability/internal/xconnect/identity", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-XConnect-Observability-Token", os.Getenv("XCONNECT_OBSERVABILITY_SEARCH_TOKEN"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: expected 400, got %d", body, rec.Code)
		}
	}
}

func TestResolveXConnectTelemetryIdentityUsesDedicatedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "general-internal-token")
	t.Setenv("XCONNECT_OBSERVABILITY_SEARCH_TOKEN", "observability-test-token-32-bytes!!")
	router := gin.New()
	RegisterRoutes(router, WithStore(store.NewMemoryStore()), WithEmailVerification(false))

	for _, tc := range []struct {
		name   string
		header string
		value  string
		want   int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "general internal token is not accepted", header: "X-Service-Token", value: "general-internal-token", want: http.StatusUnauthorized},
		{name: "dedicated token", header: "X-XConnect-Observability-Token", value: "observability-test-token-32-bytes!!", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/observability/internal/xconnect/identity", bytes.NewBufferString(`{"email":"nobody@example.test"}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
