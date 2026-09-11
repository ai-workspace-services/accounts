package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

// Register (and register/send) tell an anonymous caller whether an email
// already has an account via email_already_exists; login tells them the
// same via user_not_found. These tests pin that the resulting enumeration
// surface is bounded, on both axes (source IP and target email/identifier)
// independently, and that the two probing endpoints share one budget per
// email so switching endpoints doesn't buy a fresh one.

func registerSendRequest(t *testing.T, email, sourceIP string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sourceIP != "" {
		req.Header.Set("CF-Connecting-IP", sourceIP)
	}
	return req
}

func TestRegisterSendRateLimitedPerEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, WithEmailSender(&testEmailSender{}))

	const email = "probe-target@example.com"

	// registrationProbeEmailLimit is 5: the same email, even from
	// constantly rotating IPs, must stop answering after its own budget --
	// otherwise an attacker defeats the email axis just by rotating source
	// addresses.
	for i := 0; i < 5; i++ {
		req := registerSendRequest(t, email, fmtSourceIP(i))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d: unexpected 429 within the per-email budget: %s", i+1, rr.Body.String())
		}
	}

	req := registerSendRequest(t, email, fmtSourceIP(999))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the per-email budget is exhausted, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp.Error != "rate_limited" {
		t.Fatalf("expected error rate_limited, got %q", resp.Error)
	}
}

func TestRegisterSendRateLimitedPerIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, WithEmailSender(&testEmailSender{}))

	const sourceIP = "203.0.113.9"

	// registrationProbeIPLimit is 20: one IP sweeping distinct emails (each
	// well under its own per-email budget of 5) must still be capped by the
	// IP axis, or the per-email limit alone would let a single caller sweep
	// an unbounded email list at 4 requests each just under its trigger.
	for i := 0; i < 20; i++ {
		req := registerSendRequest(t, fmtProbeEmail(i), sourceIP)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d: unexpected 429 within the per-IP budget: %s", i+1, rr.Body.String())
		}
	}

	req := registerSendRequest(t, fmtProbeEmail(999), sourceIP)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the per-IP budget is exhausted, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterAndRegisterSendShareOneEmailBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, WithEmailSender(&testEmailSender{}))

	const email = "shared-budget@example.com"

	// Spend 4 of the 5-call email budget through register/send, then confirm
	// register() (the other endpoint answering email_already_exists) only
	// has one call left, not a fresh five -- proving the two endpoints don't
	// each get their own budget.
	for i := 0; i < 4; i++ {
		req := registerSendRequest(t, email, fmtSourceIP(i))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("register/send call %d: unexpected 429: %s", i+1, rr.Body.String())
		}
	}

	registerBody, err := json.Marshal(map[string]string{
		"name":     "Probe User",
		"email":    email,
		"password": "supersecure",
	})
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	// 5th call across both endpoints combined: still within budget.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("CF-Connecting-IP", fmtSourceIP(4))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code == http.StatusTooManyRequests {
		t.Fatalf("5th call (register): unexpected 429 within the shared budget: %s", rr.Body.String())
	}

	// 6th call: budget exhausted regardless of which endpoint reaches it.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("CF-Connecting-IP", fmtSourceIP(5))
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the shared email budget is exhausted, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLoginRateLimitedPerIdentifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router)

	const identifier = "nobody@example.com"

	// loginIdentifierAttemptLimit is 10. The account need not exist:
	// user_not_found is exactly the signal being bounded, so the limiter
	// must trip before that lookup runs.
	for i := 0; i < 10; i++ {
		body, err := json.Marshal(map[string]string{"identifier": identifier, "password": "whatever123"})
		if err != nil {
			t.Fatalf("failed to marshal payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("CF-Connecting-IP", fmtSourceIP(i))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d: unexpected 429 within the per-identifier budget: %s", i+1, rr.Body.String())
		}
		if rr.Code != http.StatusNotFound {
			t.Fatalf("call %d: expected user_not_found (404), got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}

	body, err := json.Marshal(map[string]string{"identifier": identifier, "password": "whatever123"})
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("CF-Connecting-IP", fmtSourceIP(999))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the per-identifier budget is exhausted, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp.Error != "rate_limited" {
		t.Fatalf("expected error rate_limited, got %q", resp.Error)
	}
}

// fmtSourceIP returns a distinct key per n. It is only ever used as a
// rate-limit map key (via the CF-Connecting-IP header), never parsed as a
// real address, so it need not be a valid dotted-decimal IP.
func fmtSourceIP(n int) string {
	return "198.51.100.0/" + strconv.Itoa(n)
}

func fmtProbeEmail(n int) string {
	return "probe-" + strconv.Itoa(n) + "@example.com"
}
