package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func requestContext(host string, headers map[string]string) *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	c.Request.Host = host
	for name, value := range headers {
		c.Request.Header.Set(name, value)
	}
	return c
}

// A browser discards a cookie whose Domain does not domain-match the host that
// served the response. UAT answers on *.onwalk.net while the Cloud Run config
// template still advertises accounts.svc.plus, so pinning the cookie to the
// configured domain signed the user in and then dropped the session.
func TestCookieDomainFallsBackToHostOnlyOnMismatch(t *testing.T) {
	h := &handler{publicURL: "https://accounts.svc.plus"}

	got := h.cookieDomainFor(requestContext("accounts-abc.a.run.app", map[string]string{
		"X-Forwarded-Host": "console-serverless-uat.onwalk.net",
	}))
	if got != "" {
		t.Fatalf("expected a host-only cookie for a host outside the configured domain, got %q", got)
	}
}

func TestCookieDomainKeptWhenHostBelongsToIt(t *testing.T) {
	h := &handler{publicURL: "https://accounts.svc.plus"}

	for _, host := range []string{"console.svc.plus", "svc.plus", "accounts.svc.plus:443"} {
		got := h.cookieDomainFor(requestContext("accounts-abc.a.run.app", map[string]string{
			"X-Forwarded-Host": host,
		}))
		if got != ".svc.plus" {
			t.Fatalf("expected the shared cookie domain for %q, got %q", host, got)
		}
	}
}

// The gateway forwards the browser-facing host; without that header the
// upstream host is all there is.
func TestCookieDomainUsesUpstreamHostWithoutForwardedHeader(t *testing.T) {
	h := &handler{publicURL: "https://accounts.svc.plus"}

	if got := h.cookieDomainFor(requestContext("accounts.svc.plus", nil)); got != ".svc.plus" {
		t.Fatalf("expected .svc.plus, got %q", got)
	}
	if got := h.cookieDomainFor(requestContext("accounts-serverless-uat.onwalk.net", nil)); got != "" {
		t.Fatalf("expected a host-only cookie, got %q", got)
	}
}

func TestCookieDomainEmptyWithoutPublicURL(t *testing.T) {
	h := &handler{}

	if got := h.cookieDomainFor(requestContext("console.svc.plus", nil)); got != "" {
		t.Fatalf("expected no cookie domain, got %q", got)
	}
}

// A neighbouring registrable domain must not be treated as a match.
func TestCookieDomainRejectsSuffixLookalike(t *testing.T) {
	h := &handler{publicURL: "https://accounts.svc.plus"}

	got := h.cookieDomainFor(requestContext("upstream.internal", map[string]string{
		"X-Forwarded-Host": "evilsvc.plus",
	}))
	if got != "" {
		t.Fatalf("expected a host-only cookie for a lookalike host, got %q", got)
	}
}
