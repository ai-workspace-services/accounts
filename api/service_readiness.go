package api

import (
	"net/http"
	"strings"

	"account/internal/store"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// Advanced services (dedicated node hosting, dedicated hosted resources wired to
// external clouds, etc.) require a hardened account: a verified email, a set
// password, and enabled MFA. Until all three are met the console shows an intro
// / onboarding page and activation is refused. The requirements are ordered so
// the frontend can drive a progressive "upgrade your account" flow.
const (
	readinessReqEmail    = "email"
	readinessReqPassword = "password"
	readinessReqMFA      = "mfa"
)

type serviceRequirement struct {
	Key  string `json:"key"`
	Met  bool   `json:"met"`
	Hint string `json:"hint"`
}

type serviceReadinessState struct {
	// Ready is true only when every requirement is met.
	Ready bool `json:"ready"`
	// NextStep is the key of the first unmet requirement (empty when ready),
	// so the console knows which onboarding step to surface next.
	NextStep     string               `json:"nextStep,omitempty"`
	Requirements []serviceRequirement `json:"requirements"`
}

// computeServiceReadiness derives the advanced-service prerequisites from the
// user record. Order matters: it defines the progressive onboarding sequence
// (verify email -> set password -> enable MFA).
func computeServiceReadiness(user *store.User) serviceReadinessState {
	reqs := []serviceRequirement{
		{
			Key:  readinessReqEmail,
			Met:  user.EmailVerified,
			Hint: "verify your email address",
		},
		{
			Key:  readinessReqPassword,
			Met:  strings.TrimSpace(user.PasswordHash) != "",
			Hint: "set an account password",
		},
		{
			Key:  readinessReqMFA,
			Met:  user.MFAEnabled,
			Hint: "enable two-factor authentication (MFA)",
		},
	}

	state := serviceReadinessState{Ready: true, Requirements: reqs}
	for _, r := range reqs {
		if !r.Met {
			state.Ready = false
			if state.NextStep == "" {
				state.NextStep = r.Key
			}
		}
	}
	return state
}

// serviceReadiness surfaces the advanced-service onboarding state for the
// current user so the console can render the progressive upgrade guide.
// GET /api/account/service-readiness
func (h *handler) serviceReadiness(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"readiness": computeServiceReadiness(user)})
}

// requireAdvancedServiceReadiness authenticates the caller and enforces the
// advanced-service prerequisites. Advanced-service handlers should call this
// first: on success it returns the ready user; otherwise it writes a 403
// carrying the readiness state (with intro=true so the console shows the
// onboarding/intro page instead of the service) and aborts.
func (h *handler) requireAdvancedServiceReadiness(c *gin.Context) (*store.User, bool) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return nil, false
	}
	state := computeServiceReadiness(user)
	if !state.Ready {
		c.JSON(http.StatusForbidden, gin.H{
			"error":     "advanced_service_locked",
			"message":   "complete your account security setup to activate advanced services",
			"intro":     true,
			"readiness": state,
		})
		c.Abort()
		return nil, false
	}
	return user, true
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

// setPassword lets an authenticated, email-verified user (typically an OAuth
// signup with no password yet) set a password directly — no email round trip,
// since the session already proves control of the account. This is the
// password step of the advanced-service onboarding. To rotate an existing
// password, use the password/reset flow instead.
// POST /api/auth/password/set
func (h *handler) setPassword(c *gin.Context) {
	if hasQueryParameter(c, "password") {
		respondError(c, http.StatusBadRequest, "credentials_in_query", "sensitive credentials must not be sent in the query string")
		return
	}

	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account cannot set a password")
		return
	}

	// Setting a password requires a verified email first (the ordered onboarding
	// step before this one); rotation of an existing password goes via reset.
	if !user.EmailVerified {
		respondError(c, http.StatusForbidden, "email_not_verified", "verify your email before setting a password")
		return
	}
	if strings.TrimSpace(user.PasswordHash) != "" {
		respondError(c, http.StatusConflict, "password_already_set", "a password is already set; use password reset to change it")
		return
	}

	var req setPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	password := req.Password
	if len(password) < 8 {
		respondError(c, http.StatusBadRequest, "password_too_short", "password must be at least 8 characters")
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "hash_failure", "failed to secure password")
		return
	}

	user.PasswordHash = string(hashed)
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "password_set_failed", "failed to set password")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "password set",
		"readiness": computeServiceReadiness(user),
	})
}
