package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// The verification code is six decimal digits with a ten minute TTL. Without a
// cap on guesses that space is sweepable, and the already-registered branch of
// verifyEmail hands back a session on success -- so the cap is what stands
// between a guess loop and an existing account.

func postJSON(t *testing.T, router *gin.Engine, path string, payload map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// wrongCode returns a six digit code that is not the real one, so a test never
// passes by accidentally guessing right.
func wrongCode(real string) string {
	if real != "000000" {
		return "000000"
	}
	return "111111"
}

func newVerificationHarness(t *testing.T) (*gin.Engine, *testEmailSender) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	mailer := &testEmailSender{}
	RegisterRoutes(router, WithEmailSender(mailer))
	return router, mailer
}

func issueCode(t *testing.T, router *gin.Engine, mailer *testEmailSender, email string) string {
	t.Helper()
	if rec := postJSON(t, router, "/api/auth/register/send", map[string]string{"email": email}); rec.Code != http.StatusOK {
		t.Fatalf("expected verification send to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	msg, ok := mailer.last()
	if !ok {
		t.Fatal("expected a verification email to be sent")
	}
	return extractVerificationCodeFromMessage(t, msg)
}

func TestRegistrationVerificationLocksAfterRepeatedWrongCodes(t *testing.T) {
	router, mailer := newVerificationHarness(t)
	const email = "guessed@example.com"
	real := issueCode(t, router, mailer, email)
	bad := wrongCode(real)

	// Attempts up to the last one report a plain wrong code. The final attempt
	// both fails and spends the budget, and is answered with the lockout --
	// same shape as verifyMFALogin.
	for i := 0; i < maxEmailVerificationAttempts-1; i++ {
		rec := postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": bad})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: expected 400 for a wrong code, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": bad})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the budget-spending attempt to lock, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeResponse(t, rec).Error; got != "verification_locked" {
		t.Fatalf("expected error verification_locked, got %q", got)
	}
}

// The lockout has to refuse the *right* code too. A lockout that only rejects
// wrong guesses would still let an attacker land the moment they hit the
// correct one, which is the only guess that matters.
func TestVerificationLockoutRefusesTheCorrectCode(t *testing.T) {
	router, mailer := newVerificationHarness(t)
	const email = "locked-out@example.com"
	real := issueCode(t, router, mailer, email)
	bad := wrongCode(real)

	for i := 0; i < maxEmailVerificationAttempts; i++ {
		postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": bad})
	}

	rec := postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": real})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the correct code to be refused while locked, got %d: %s", rec.Code, rec.Body.String())
	}
}

// sendEmailVerification reuses the existing code while it is unexpired, so a
// resend must not refill the guess budget -- otherwise the lockout is one
// extra request away from being bypassed indefinitely.
func TestResendDoesNotRefillVerificationBudget(t *testing.T) {
	router, mailer := newVerificationHarness(t)
	const email = "resender@example.com"
	real := issueCode(t, router, mailer, email)
	bad := wrongCode(real)

	for i := 0; i < maxEmailVerificationAttempts; i++ {
		postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": bad})
	}

	if rec := postJSON(t, router, "/api/auth/register/send", map[string]string{"email": email}); rec.Code != http.StatusOK {
		t.Fatalf("expected resend to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	rec := postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": bad})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("resend refilled the budget: expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCorrectCodeStillVerifiesWithinBudget(t *testing.T) {
	router, mailer := newVerificationHarness(t)
	const email = "honest@example.com"
	real := issueCode(t, router, mailer, email)
	bad := wrongCode(real)

	// A couple of genuine typos must not cost the user their code.
	for i := 0; i < maxEmailVerificationAttempts-1; i++ {
		postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": bad})
	}

	rec := postJSON(t, router, "/api/auth/register/verify", map[string]string{"email": email, "code": real})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the correct code to verify within budget, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The tests above exercise the mechanism but take their loop bound from the
// constant, so they hold for any budget -- including one large enough to be no
// protection at all. This one pins the security property itself, in the terms
// that make the gap real: how many guesses an attacker gets at a 10^6 space
// before the code expires.
func TestVerificationBudgetLeavesTheCodeSpaceUnsweepable(t *testing.T) {
	const codeSpace = 1000000 // six decimal digits

	// A lockout does not end the attempt; it pauses it. Across the code's TTL
	// the attacker gets one budget per lockout window, plus the first one.
	windows := int(defaultEmailVerificationTTL/emailVerificationLockoutDuration) + 1
	guesses := windows * maxEmailVerificationAttempts

	// One in a thousand is already generous for a single targeted account.
	if odds := float64(guesses) / float64(codeSpace); odds > 0.001 {
		t.Fatalf(
			"a code is guessable with probability %.4f (%d guesses over %d windows against %d codes); "+
				"tighten maxEmailVerificationAttempts (%d), lengthen emailVerificationLockoutDuration (%s), "+
				"shorten defaultEmailVerificationTTL (%s), or widen the code space",
			odds, guesses, windows, codeSpace,
			maxEmailVerificationAttempts, emailVerificationLockoutDuration, defaultEmailVerificationTTL,
		)
	}
}
