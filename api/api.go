package api

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/auth"
	"account/internal/service"
	"account/internal/store"
)

const defaultSessionTTL = 24 * time.Hour
const defaultMFAChallengeTTL = 10 * time.Minute
const defaultTOTPIssuer = "XControl Account"
const defaultEmailVerificationTTL = 10 * time.Minute
const defaultPasswordResetTTL = 30 * time.Minute
const maxMFAVerificationAttempts = 5
const defaultMFALockoutDuration = 5 * time.Minute
const defaultOAuthExchangeCodeTTL = 5 * time.Minute

const sessionCookieName = "xc_session"

type imageVersionInfo struct {
	ImageRef string `json:"image_ref"`
	Tag      string `json:"tag,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Version  string `json:"version,omitempty"`
}

type session struct {
	userID    string
	expiresAt time.Time
}

type oauthExchangeCode struct {
	sessionToken     string
	sessionExpiresAt time.Time
	expiresAt        time.Time
}

type handler struct {
	store                     store.Store
	mu                        sync.RWMutex
	sessionTTL                time.Duration
	mfaChallenges             map[string]mfaChallenge
	mfaMu                     sync.RWMutex
	mfaChallengeTTL           time.Duration
	totpIssuer                string
	emailSender               EmailSender
	emailVerificationEnabled  bool
	verificationTTL           time.Duration
	verifications             map[string]emailVerification
	verificationMu            sync.RWMutex
	registrationVerifications map[string]registrationVerification
	registrationMu            sync.RWMutex
	resetTTL                  time.Duration
	passwordResets            map[string]passwordReset
	resetMu                   sync.RWMutex
	oauthExchangeCodes        map[string]oauthExchangeCode
	oauthExchangeMu           sync.RWMutex
	oauthExchangeTTL          time.Duration
	metricsProvider           service.UserMetricsProvider
	agentStatusReader         agentStatusReader
	tokenService              *auth.TokenService
	oauthProviders            map[string]auth.OAuthProvider
	oauthFrontendURL          string
	publicURL                 string
	xworkmateVaultService     xworkmateVaultService
	xrayConfigRenderer        func(*store.User) (string, string, []string, error)
	agentRegistry             agentRegistry
	db                        *gorm.DB
	stripe                    *stripeClient
}

type agentRegistry interface {
	IsSandboxAgent(agentID string) bool
	SetSandboxAgent(agentID string, enabled bool)
	ClearSandboxAgents()

	Authenticate(token string) (*agentserver.Identity, bool)
	RegisterAgent(agentID string, groups []string) agentserver.Identity
	ReportStatus(agent agentserver.Identity, report agentproto.StatusReport)
}

type mfaChallenge struct {
	userID         string
	expiresAt      time.Time
	totpSecret     string
	totpIssuer     string
	totpAccount    string
	totpIssuedAt   time.Time
	failedAttempts int
	lockedUntil    time.Time
}

type emailVerification struct {
	userID    string
	email     string
	code      string
	expiresAt time.Time
}

type passwordReset struct {
	userID    string
	email     string
	expiresAt time.Time
}

type registrationVerification struct {
	email     string
	code      string
	expiresAt time.Time
	verified  bool
}

// Option configures handler behaviour when registering routes.
type Option func(*handler)

// WithStore overrides the default in-memory store with the provided implementation.
func WithStore(st store.Store) Option {
	return func(h *handler) {
		if st != nil {
			h.store = st
		}
	}
}

// WithSessionTTL sets the TTL used for issued sessions.
func WithSessionTTL(ttl time.Duration) Option {
	return func(h *handler) {
		if ttl > 0 {
			h.sessionTTL = ttl
		}
	}
}

// WithEmailSender configures the handler to use the provided EmailSender for outbound notifications.
func WithEmailSender(sender EmailSender) Option {
	return func(h *handler) {
		if sender != nil {
			h.emailSender = sender
		}
	}
}

// WithEmailVerification configures whether user registration requires email verification.
func WithEmailVerification(enabled bool) Option {
	return func(h *handler) {
		h.emailVerificationEnabled = enabled
	}
}

// WithEmailVerificationTTL overrides the default TTL for email verification tokens.
func WithEmailVerificationTTL(ttl time.Duration) Option {
	return func(h *handler) {
		if ttl > 0 {
			h.verificationTTL = ttl
		}
	}
}

// WithUserMetricsProvider configures the handler with the provided metrics provider.
func WithUserMetricsProvider(provider service.UserMetricsProvider) Option {
	return func(h *handler) {
		if provider != nil {
			h.metricsProvider = provider
		}
	}
}

// WithAgentStatusReader wires the agent status reader used by admin endpoints.
func WithAgentStatusReader(reader agentStatusReader) Option {
	return func(h *handler) {
		if reader != nil {
			h.agentStatusReader = reader
		}
	}
}

// WithPasswordResetTTL overrides the default TTL for password reset tokens.
func WithPasswordResetTTL(ttl time.Duration) Option {
	return func(h *handler) {
		if ttl > 0 {
			h.resetTTL = ttl
		}
	}
}

// WithTokenService configures the handler with the provided token service.
func WithTokenService(tokenService *auth.TokenService) Option {
	return func(h *handler) {
		if tokenService != nil {
			h.tokenService = tokenService
		}
	}
}

// WithOAuthProviders configures the handler with the provided OAuth2 providers.
func WithOAuthProviders(providers map[string]auth.OAuthProvider) Option {
	return func(h *handler) {
		h.oauthProviders = providers
	}
}

// WithServerPublicURL configures the public URL of the account service.
func WithServerPublicURL(url string) Option {
	return func(h *handler) {
		h.publicURL = url
	}
}

// WithXrayConfigRenderer overrides sync config rendering.
// It exists primarily to make sync endpoint behavior testable.
func WithXrayConfigRenderer(renderer func(*store.User) (string, string, []string, error)) Option {
	return func(h *handler) {
		if renderer != nil {
			h.xrayConfigRenderer = renderer
		}
	}
}

// WithOAuthFrontendURL configures the frontend URL for OAuth2 redirects.
func WithOAuthFrontendURL(url string) Option {
	return func(h *handler) {
		h.oauthFrontendURL = url
	}
}

// WithXWorkmateVaultService configures the Vault-backed secret service used by
// xworkmate integration endpoints.
func WithXWorkmateVaultService(vaultService xworkmateVaultService) Option {
	return func(h *handler) {
		h.xworkmateVaultService = vaultService
	}
}

// WithAgentRegistry configures the handler with the provided agent registry.
func WithAgentRegistry(registry agentRegistry) Option {
	return func(h *handler) {
		if isNilAgentRegistry(registry) {
			return
		}
		h.agentRegistry = registry
	}
}

func isNilAgentRegistry(registry agentRegistry) bool {
	if registry == nil {
		return true
	}
	value := reflect.ValueOf(registry)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// WithGormDB configures the handler with the provided GORM database for admin settings.
func WithGormDB(db *gorm.DB) Option {
	return func(h *handler) {
		h.db = db
	}
}

// WithStripeConfig configures Stripe billing integration.
func WithStripeConfig(cfg StripeConfig) Option {
	return func(h *handler) {
		h.stripe = newStripeClient(cfg)
	}
}

// RegisterRoutes attaches account service endpoints to the router.
func RegisterRoutes(r *gin.Engine, opts ...Option) {
	h := &handler{
		store:                     store.NewMemoryStore(),
		sessionTTL:                defaultSessionTTL,
		mfaChallenges:             make(map[string]mfaChallenge),
		mfaChallengeTTL:           defaultMFAChallengeTTL,
		totpIssuer:                defaultTOTPIssuer,
		emailSender:               noopEmailSender,
		emailVerificationEnabled:  true,
		verificationTTL:           defaultEmailVerificationTTL,
		verifications:             make(map[string]emailVerification),
		registrationVerifications: make(map[string]registrationVerification),
		resetTTL:                  defaultPasswordResetTTL,
		passwordResets:            make(map[string]passwordReset),
		oauthExchangeCodes:        make(map[string]oauthExchangeCode),
		oauthExchangeTTL:          defaultOAuthExchangeCodeTTL,
	}

	for _, opt := range opts {
		opt(h)
	}

	if h.tokenService != nil && h.store != nil {
		h.tokenService.SetStore(h.store)
	}

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/api/ping", func(c *gin.Context) {
		info := parseImageVersionInfo(os.Getenv("IMAGE"))
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"image":   info.ImageRef,
			"tag":     info.Tag,
			"commit":  info.Commit,
			"version": info.Version,
		})
	})

	authGroup := r.Group("/api/auth")

	authGroup.POST("/register", h.register)
	authGroup.POST("/register/verify", h.verifyEmail)
	authGroup.POST("/register/send", h.sendEmailVerification)

	authGroup.POST("/login", h.login)
	authGroup.POST("/mfa/verify", h.verifyMFALogin)

	// Token exchange endpoint - converts one-time OAuth exchange code to a real session token.
	authGroup.POST("/token/exchange", h.exchangeToken)

	// OAuth2 routes
	authGroup.GET("/oauth/login/:provider", h.oauthLogin)
	authGroup.GET("/oauth/callback/:provider", h.oauthCallback)

	// Token refresh endpoint - generates new access token using refresh token
	authGroup.POST("/token/refresh", h.refreshToken)
	authGroup.POST("/refresh", h.refreshToken)

	authGroup.GET("/mfa/status", h.mfaStatus)
	authGroup.GET("/sync/config", h.syncConfigSnapshot)
	authGroup.POST("/sync/ack", h.syncConfigAck)
	authGroup.GET("/homepage-video", h.getHomepageVideoPublic)

	// Sandbox binding read endpoint.
	// Used by the Console Guest/Demo experience. Must be readable either via a
	// normal user session or via the internal service token.
	authGroup.GET("/sandbox/binding", h.getSandboxBindingPublic)

	// Protected routes requiring authentication
	authProtected := authGroup.Group("")
	if h.tokenService != nil {
		authProtected.Use(h.tokenService.AuthMiddleware())
		authProtected.Use(auth.RequireActiveUser(h.store))
	}

	authProtected.GET("/session", h.session)
	authProtected.DELETE("/session", h.deleteSession)
	authProtected.GET("/xworkmate/profile", h.getXWorkmateProfile)
	authProtected.GET("/xworkmate/profile/sync", h.getXWorkmateProfileSync)
	authProtected.PUT("/xworkmate/profile", h.updateXWorkmateProfile)
	authProtected.GET("/xworkmate/secrets", h.getXWorkmateSecrets)
	authProtected.PUT("/xworkmate/secrets/:target", h.putXWorkmateSecret)
	authProtected.DELETE("/xworkmate/secrets/:target", h.deleteXWorkmateSecret)

	authProtected.POST("/mfa/totp/provision", h.provisionTOTP)
	authProtected.POST("/mfa/totp/verify", h.verifyTOTP)
	authProtected.POST("/mfa/disable", h.disableMFA)

	authProtected.POST("/password/reset", h.requestPasswordReset)
	authProtected.POST("/password/reset/confirm", h.confirmPasswordReset)

	authProtected.GET("/subscriptions", h.listSubscriptions)
	authProtected.POST("/subscriptions", h.upsertSubscription)
	authProtected.POST("/subscriptions/cancel", h.cancelSubscription)
	authProtected.POST("/stripe/checkout", h.stripeCheckout)
	authProtected.POST("/stripe/portal", h.stripePortal)

	authProtected.POST("/config/sync", h.syncConfig)

	authProtected.GET("/admin/settings", h.getAdminSettings)
	authProtected.POST("/admin/settings", h.updateAdminSettings)
	authProtected.GET("/admin/homepage-video", h.getHomepageVideoSettings)
	authProtected.PUT("/admin/homepage-video", h.updateHomepageVideoSettings)

	// Backward-compatible auth-scoped admin routes consumed by the dashboard BFF.
	authProtected.GET("/admin/users/metrics", h.adminUsersMetrics)
	authProtected.POST("/admin/users", h.createCustomUser)
	authProtected.POST("/admin/users/:userId/role", h.updateUserRole)
	authProtected.DELETE("/admin/users/:userId/role", h.resetUserRole)
	authProtected.POST("/admin/users/:userId/pause", h.pauseUser)
	authProtected.POST("/admin/users/:userId/resume", h.resumeUser)
	authProtected.DELETE("/admin/users/:userId", h.deleteUser)
	authProtected.POST("/admin/users/:userId/renew-uuid", h.renewProxyUUID)
	authProtected.POST("/admin/tenants/bootstrap", h.bootstrapTenant)
	authProtected.GET("/admin/blacklist", h.listBlacklist)
	authProtected.POST("/admin/blacklist", h.addToBlacklist)
	authProtected.DELETE("/admin/blacklist/:email", h.removeFromBlacklist)

	// Sandbox node binding (root-only via permissions guard).
	authProtected.GET("/admin/sandbox/binding", h.getSandboxBinding)
	authProtected.POST("/admin/sandbox/bind", h.bindSandboxNode)

	// Root-only identity switch to sandbox@svc.plus (hard-coded allowlist).
	authProtected.POST("/admin/assume", h.adminAssume)
	authProtected.POST("/admin/assume/revert", h.adminAssumeRevert)
	authProtected.GET("/admin/assume/status", h.adminAssumeStatus)

	authProtected.GET("/users", h.listUsers)

	// Internal routes for service-to-service reads.
	internalGroup := r.Group("/api/internal")

	r.POST("/api/billing/stripe/webhook", h.stripeWebhook)
	internalGroup.Use(auth.InternalAuthMiddleware())
	internalGroup.GET("/public-overview", h.internalPublicOverview)
	internalGroup.GET("/sandbox/guest", h.internalSandboxGuest)
	internalGroup.GET("/network/identities", h.internalNetworkIdentities)
	internalGroup.GET("/policy/:accountUUID", h.internalAccountPolicy)
	internalGroup.POST("/nodes/heartbeat", h.internalNodeHeartbeat)

	// Public /api routes for admin/management (expected by frontend at /api/admin/...)
	apiGroup := r.Group("/api")
	if h.tokenService != nil {
		apiGroup.Use(h.tokenService.AuthMiddleware())
		apiGroup.Use(auth.RequireActiveUser(h.store))
	}
	registerAdminRoutes(apiGroup, h)

	// Canonical user-facing agent routes.
	// These endpoints use session-based auth in handler logic and intentionally
	// stay outside token middleware to support dashboard session tokens.
	agentServerGroup := r.Group("/api/agent-server/v1")
	agentServerGroup.GET("/nodes", h.listAgentNodes)
	agentServerGroup.GET("/users", h.listAgentUsers)
	agentServerGroup.POST("/status", h.reportAgentStatus)

	accountGroup := r.Group("/api/account")
	accountGroup.GET("/usage/summary", h.accountUsageSummary)
	accountGroup.GET("/usage/buckets", h.accountUsageBuckets)
	accountGroup.GET("/billing/summary", h.accountBillingSummary)
	accountGroup.GET("/policy", h.accountPolicy)

	// Legacy alias kept for backward compatibility.
	agentGroup := r.Group("/api/agent")
	agentGroup.GET("/nodes", h.listAgentNodes)
}

type registerRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

type loginRequest struct {
	Identifier string `json:"identifier"`
	Account    string `json:"account"`
	Username   string `json:"username"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	TOTPCode   string `json:"totpCode"`
}

type verificationCodeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type verificationSendRequest struct {
	Email string `json:"email"`
}

type passwordResetRequestBody struct {
	Email string `json:"email"`
}

type passwordResetConfirmRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

type subscriptionUpsertRequest struct {
	ExternalID    string         `json:"externalId"`
	Provider      string         `json:"provider"`
	PaymentMethod string         `json:"paymentMethod"`
	PaymentQRCode string         `json:"paymentQr"`
	Kind          string         `json:"kind"`
	PlanID        string         `json:"planId"`
	Status        string         `json:"status"`
	Meta          map[string]any `json:"meta"`
}

type subscriptionCancelRequest struct {
	ExternalID string `json:"externalId"`
}

func hasQueryParameter(c *gin.Context, keys ...string) bool {
	if len(keys) == 0 {
		return false
	}

	values := c.Request.URL.Query()
	for _, key := range keys {
		if _, ok := values[key]; ok {
			return true
		}
	}

	return false
}

func (h *handler) register(c *gin.Context) {
	if hasQueryParameter(c, "password", "email", "confirmPassword") {
		respondError(c, http.StatusBadRequest, "credentials_in_query", "sensitive credentials must not be sent in the query string")
		return
	}

	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	name := strings.TrimSpace(req.Name)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := strings.TrimSpace(req.Password)
	code := strings.TrimSpace(req.Code)

	if name == "" {
		respondError(c, http.StatusBadRequest, "name_required", "name is required")
		return
	}

	if email == "" || password == "" {
		respondError(c, http.StatusBadRequest, "missing_credentials", "email and password are required")
		return
	}

	blacklisted, err := h.store.IsBlacklisted(c.Request.Context(), email)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "blacklist_check_failed", "failed to verify email status")
		return
	}
	if blacklisted {
		respondError(c, http.StatusForbidden, "email_blacklisted", "this email address is blocked")
		return
	}

	if !strings.Contains(email, "@") {
		respondError(c, http.StatusBadRequest, "invalid_email", "email must be a valid address")
		return
	}

	if len(password) < 8 {
		respondError(c, http.StatusBadRequest, "password_too_short", "password must be at least 8 characters")
		return
	}

	if h.emailVerificationEnabled {
		if code == "" {
			respondError(c, http.StatusBadRequest, "verification_required", "verification code is required")
			return
		}

		verification, ok := h.lookupRegistrationVerification(email)
		if !ok {
			respondError(c, http.StatusBadRequest, "verification_required", "verification code is required")
			return
		}

		if verification.code != code {
			respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
			return
		}
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "hash_failure", "failed to secure password")
		return
	}

	user := &store.User{
		Name:         name,
		Email:        email,
		PasswordHash: string(hashed),
		Level:        store.LevelUser,
		Role:         store.RoleUser,
		Groups:       []string{"User"},
	}

	if !h.emailVerificationEnabled || code != "" {
		user.EmailVerified = true
	}

	if err := h.store.CreateUser(c.Request.Context(), user); err != nil {
		switch {
		case errors.Is(err, store.ErrEmailExists):
			respondError(c, http.StatusConflict, "email_already_exists", "user with this email already exists")
			return
		case errors.Is(err, store.ErrNameExists):
			respondError(c, http.StatusConflict, "name_already_exists", "user with this name already exists")
			return
		case errors.Is(err, store.ErrInvalidName):
			respondError(c, http.StatusBadRequest, "invalid_name", "name is invalid")
			return
		default:
			respondError(c, http.StatusInternalServerError, "user_creation_failed", "failed to create user")
			return
		}
	}

	if h.emailVerificationEnabled {
		h.removeRegistrationVerification(email)
	}

	trialExpiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	trial := &store.Subscription{
		UserID:        user.ID,
		Provider:      "trial",
		PaymentMethod: "trial",
		Kind:          "trial",
		PlanID:        "TRIAL-7D",
		ExternalID:    fmt.Sprintf("trial-%s", user.ID),
		Status:        "active",
		Meta: map[string]any{
			"startsAt":  time.Now().UTC(),
			"expiresAt": trialExpiresAt,
			"note":      "new user full-access trial",
		},
	}

	if err := h.store.UpsertSubscription(c.Request.Context(), trial); err != nil {
		slog.Warn("failed to provision onboarding trial", "err", err, "userID", user.ID)
	}

	message := "registration successful"

	response := gin.H{
		"message": message,
		"user":    sanitizeUser(user, nil),
	}
	c.JSON(http.StatusCreated, response)
}

func (h *handler) verifyEmail(c *gin.Context) {
	if hasQueryParameter(c, "token", "code") {
		respondError(c, http.StatusBadRequest, "token_in_query", "verification code must be sent in the request body")
		return
	}

	var req verificationCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	code := strings.TrimSpace(req.Code)

	if email == "" || code == "" {
		respondError(c, http.StatusBadRequest, "invalid_request", "email and verification code are required")
		return
	}

	if len(code) != 6 {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code must be 6 digits")
		return
	}

	for _, r := range code {
		if r < '0' || r > '9' {
			respondError(c, http.StatusBadRequest, "invalid_code", "verification code must be 6 digits")
			return
		}
	}

	if verification, ok := h.lookupEmailVerification(email); ok {
		if verification.code != code {
			respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
			return
		}

		user, err := h.store.GetUserByID(c.Request.Context(), verification.userID)
		if err != nil {
			slog.Error("failed to load user for email verification", "err", err, "userID", verification.userID)
			respondError(c, http.StatusInternalServerError, "verification_failed", "failed to verify email")
			return
		}

		if !strings.EqualFold(strings.TrimSpace(user.Email), verification.email) {
			h.removeEmailVerification(email)
			respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
			return
		}

		if !user.EmailVerified {
			user.EmailVerified = true
			if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
				slog.Error("failed to update user during email verification", "err", err, "userID", user.ID)
				respondError(c, http.StatusInternalServerError, "verification_failed", "failed to verify email")
				return
			}
		}

		h.removeEmailVerification(email)

		sessionToken, expiresAt, err := h.createSession(user.ID)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
			return
		}

		h.setSessionCookie(c, sessionToken, expiresAt)

		c.JSON(http.StatusOK, gin.H{
			"message":   "email verified",
			"token":     sessionToken,
			"expiresAt": expiresAt.UTC(),
			"user":      sanitizeUser(user, nil),
		})
		return
	}

	pending, ok := h.lookupRegistrationVerification(email)
	if !ok || pending.code != code {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}

	if !h.markRegistrationVerified(email) {
		respondError(c, http.StatusBadRequest, "invalid_code", "verification code is invalid or expired")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "verification successful", "verified": true})
}

func (h *handler) sendEmailVerification(c *gin.Context) {
	if hasQueryParameter(c, "email") {
		respondError(c, http.StatusBadRequest, "email_in_query", "email must be sent in the request body")
		return
	}

	if !h.emailVerificationEnabled {
		c.JSON(http.StatusOK, gin.H{"message": "verification email sent"})
		return
	}

	var req verificationSendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		respondError(c, http.StatusBadRequest, "invalid_email", "email must be a valid address")
		return
	}

	// 与线上 SMTP 配置对齐：统一使用 10s 的超时控制
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// 基础邮箱校验，避免明显无效地址触发外发
	if !strings.Contains(email, "@") {
		respondError(c, http.StatusBadRequest, "invalid_email", "email must be a valid address")
		return
	}

	blacklisted, err := h.store.IsBlacklisted(c.Request.Context(), email)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "blacklist_check_failed", "failed to verify email status")
		return
	}
	if blacklisted {
		respondError(c, http.StatusForbidden, "email_blacklisted", "this email address is blocked")
		return
	}

	user, err := h.store.GetUserByEmail(ctx, email)
	if err == nil {
		if strings.TrimSpace(user.Email) == "" {
			respondError(c, http.StatusBadRequest, "invalid_email", "email must be a valid address")
			return
		}

		if user.EmailVerified {
			respondError(c, http.StatusConflict, "email_already_exists", "email is already registered")
			return
		}

		if err := h.enqueueEmailVerification(ctx, user); err != nil {
			slog.Error("failed to send verification email", "err", err, "email", user.Email)
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				respondError(c, http.StatusGatewayTimeout, "smtp_timeout", "email sending timed out")
			} else {
				respondError(c, http.StatusInternalServerError, "verification_failed", "verification email could not be sent")
			}
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "verification email sent"})
		return
	}

	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		respondError(c, http.StatusInternalServerError, "verification_failed", "verification email could not be sent")
		return
	}

	if _, err := h.issueRegistrationVerification(ctx, email); err != nil {
		slog.Error("failed to issue registration verification", "err", err, "email", email)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			respondError(c, http.StatusGatewayTimeout, "smtp_timeout", "email sending timed out")
		} else {
			respondError(c, http.StatusInternalServerError, "verification_failed", "verification email could not be sent")
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "verification email sent"})
}

func (h *handler) requestPasswordReset(c *gin.Context) {
	if hasQueryParameter(c, "email") {
		respondError(c, http.StatusBadRequest, "email_in_query", "email must be sent in the request body")
		return
	}

	var req passwordResetRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		respondError(c, http.StatusBadRequest, "email_required", "email is required")
		return
	}

	user, err := h.store.GetUserByEmail(c.Request.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			c.JSON(http.StatusAccepted, gin.H{"message": "if the account exists a reset email will be sent"})
			return
		}
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to initiate password reset")
		return
	}

	if strings.TrimSpace(user.Email) == "" || !user.EmailVerified {
		c.JSON(http.StatusAccepted, gin.H{"message": "if the account exists a reset email will be sent"})
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account cannot change password")
		return
	}

	if err := h.enqueuePasswordReset(c.Request.Context(), user); err != nil {
		slog.Error("failed to send password reset email", "err", err, "email", user.Email)
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to initiate password reset")
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"message": "if the account exists a reset email will be sent"})
}

func (h *handler) confirmPasswordReset(c *gin.Context) {
	if hasQueryParameter(c, "token", "password") {
		respondError(c, http.StatusBadRequest, "credentials_in_query", "sensitive credentials must not be sent in the query string")
		return
	}

	var req passwordResetConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	token := strings.TrimSpace(req.Token)
	password := strings.TrimSpace(req.Password)

	if token == "" || password == "" {
		respondError(c, http.StatusBadRequest, "invalid_request", "token and password are required")
		return
	}

	if len(password) < 8 {
		respondError(c, http.StatusBadRequest, "password_too_short", "password must be at least 8 characters")
		return
	}

	reset, ok := h.lookupPasswordReset(token)
	if !ok {
		respondError(c, http.StatusBadRequest, "invalid_token", "reset token is invalid or expired")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), reset.userID)
	if err != nil {
		slog.Error("failed to load user for password reset", "err", err, "userID", reset.userID)
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}

	if !strings.EqualFold(strings.TrimSpace(user.Email), reset.email) {
		h.removePasswordReset(token)
		respondError(c, http.StatusBadRequest, "invalid_token", "reset token is invalid or expired")
		return
	}
	if h.isReadOnlyAccount(user) {
		h.removePasswordReset(token)
		respondError(c, http.StatusForbidden, "read_only_account", "demo account cannot change password")
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}

	user.PasswordHash = string(hashed)
	user.EmailVerified = true
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		slog.Error("failed to update user during password reset", "err", err, "userID", user.ID)
		respondError(c, http.StatusInternalServerError, "password_reset_failed", "failed to reset password")
		return
	}

	h.removePasswordReset(token)

	sessionToken, expiresAt, err := h.createSession(user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
		return
	}

	h.setSessionCookie(c, sessionToken, expiresAt)

	c.JSON(http.StatusOK, gin.H{
		"message":   "password reset successful",
		"token":     sessionToken,
		"expiresAt": expiresAt.UTC(),
		"user":      sanitizeUser(user, nil),
	})
}

var allowedPermissionMatrixRoles = map[string]struct{}{
	store.RoleRoot:     {},
	store.RoleOperator: {},
	store.RoleUser:     {},
	store.RoleReadOnly: {},
	store.RoleAdmin:    {},
}

var assignableUserRoles = map[string]struct{}{
	store.RoleOperator: {},
	store.RoleUser:     {},
	store.RoleReadOnly: {},
}

func (h *handler) getAdminSettings(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsRead); !ok {
		return
	}
	settings, err := service.GetAdminSettings(c.Request.Context())
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, service.ErrServiceDBNotInitialized) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"version": settings.Version,
		"matrix":  settings.Matrix,
	})
}

func (h *handler) updateAdminSettings(c *gin.Context) {
	adminUser, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(adminUser) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	var req struct {
		Version uint64                     `json:"version"`
		Matrix  map[string]map[string]bool `json:"matrix"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	normalized, err := normalizeAdminMatrix(req.Matrix)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updated, err := service.SaveAdminSettings(c.Request.Context(), service.AdminSettings{
		Version: req.Version,
		Matrix:  normalized,
	})
	if err != nil {
		if errors.Is(err, service.ErrAdminSettingsVersionConflict) {
			c.JSON(http.StatusConflict, gin.H{
				"error":   err.Error(),
				"version": updated.Version,
				"matrix":  updated.Matrix,
			})
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, service.ErrServiceDBNotInitialized) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"version": updated.Version,
		"matrix":  updated.Matrix,
	})
}

func normalizeAdminMatrix(in map[string]map[string]bool) (map[string]map[string]bool, error) {
	if in == nil {
		return make(map[string]map[string]bool), nil
	}
	out := make(map[string]map[string]bool, len(in))
	for module, roles := range in {
		moduleKey := strings.TrimSpace(module)
		if moduleKey == "" {
			return nil, errors.New("module key cannot be empty")
		}
		if roles == nil {
			out[moduleKey] = make(map[string]bool)
			continue
		}
		normalizedRoles := make(map[string]bool, len(roles))
		for role, enabled := range roles {
			key := strings.ToLower(strings.TrimSpace(role))
			if key == "" {
				return nil, errors.New("role cannot be empty")
			}
			if _, ok := allowedPermissionMatrixRoles[key]; !ok {
				return nil, fmt.Errorf("unsupported role: %s", role)
			}
			normalizedRoles[key] = enabled
		}
		out[moduleKey] = normalizedRoles
	}
	return out, nil
}

func (h *handler) login(c *gin.Context) {
	if hasQueryParameter(c, "username", "password", "identifier", "totp") {
		respondError(c, http.StatusBadRequest, "credentials_in_query", "sensitive credentials must not be sent in the query string")
		return
	}

	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(req.Account)
	}
	if identifier == "" {
		identifier = strings.TrimSpace(req.Username)
	}
	if identifier == "" {
		identifier = strings.TrimSpace(req.Email)
	}

	password := strings.TrimSpace(req.Password)
	totpCode := strings.TrimSpace(req.TOTPCode)

	if identifier == "" {
		respondError(c, http.StatusBadRequest, "missing_credentials", "identifier is required")
		return
	}

	user, err := h.findUserByIdentifier(c.Request.Context(), identifier)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "authentication_failed", "failed to authenticate user")
		return
	}

	// Sandbox user is not allowed to login by password/totp.
	// Root can only assume into sandbox via the admin assume endpoint.
	if strings.EqualFold(strings.TrimSpace(user.Email), sandboxUserEmail) {
		respondError(c, http.StatusForbidden, "sandbox_no_login", "sandbox login is disabled")
		return
	}

	if password != "" {
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
			respondError(c, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
			return
		}
	} else {
		if totpCode == "" {
			respondError(c, http.StatusBadRequest, "missing_credentials", "totp code is required")
			return
		}
		if !strings.EqualFold(strings.TrimSpace(user.Email), identifier) {
			respondError(c, http.StatusUnauthorized, "password_required", "password required for this identifier")
			return
		}
	}

	if strings.TrimSpace(user.Email) != "" && !user.EmailVerified {
		respondError(c, http.StatusUnauthorized, "email_not_verified", "email must be verified before login")
		return
	}

	// Demo/read-only account explicitly disables MFA to keep the roaming
	// experience simple while write operations remain blocked by policy.
	if h.isReadOnlyAccount(user) {
		if user.MFAEnabled || strings.TrimSpace(user.MFATOTPSecret) != "" || !user.MFASecretIssuedAt.IsZero() || !user.MFAConfirmedAt.IsZero() {
			user.MFATOTPSecret = ""
			user.MFAEnabled = false
			user.MFASecretIssuedAt = time.Time{}
			user.MFAConfirmedAt = time.Time{}
			if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
				slog.Warn("failed to reset mfa state for read-only account", "err", err, "userID", user.ID)
			}
		}
	}

	if user.MFAEnabled {
		if totpCode == "" {
			mfaTicket, err := h.createMFAChallenge(user.ID)
			if err != nil {
				respondError(c, http.StatusInternalServerError, "mfa_challenge_creation_failed", "failed to create mfa challenge")
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"message":      "mfa required",
				"mfaRequired":  true,
				"mfa_required": true,
				"mfaMethod":    "totp",
				"mfa_method":   "totp",
				"mfaTicket":    mfaTicket,
				"mfa_ticket":   mfaTicket,
				// Kept for backward compatibility with existing clients.
				"mfaToken": mfaTicket,
			})
			return
		}

		valid, err := totp.ValidateCustom(totpCode, user.MFATOTPSecret, time.Now().UTC(), totp.ValidateOpts{
			Period:    30,
			Skew:      1,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			respondError(c, http.StatusInternalServerError, "invalid_mfa_code", "invalid totp code")
			return
		}
		if !valid {
			respondError(c, http.StatusUnauthorized, "invalid_mfa_code", "invalid totp code")
			return
		}

		token, expiresAt, err := h.createSession(user.ID)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
			return
		}

		h.setSessionCookie(c, token, expiresAt)

		c.JSON(http.StatusOK, gin.H{
			"message":      "login successful",
			"token":        token,
			"access_token": token,
			"expiresAt":    expiresAt.UTC(),
			"expires_in":   int64(time.Until(expiresAt).Seconds()),
			"mfaRequired":  false,
			"mfa_required": false,
			"user":         sanitizeUser(user, nil),
		})
		return
	}

	token, expiresAt, err := h.createSession(user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
		return
	}

	h.setSessionCookie(c, token, expiresAt)

	response := gin.H{
		"message":      "login successful",
		"token":        token,
		"access_token": token,
		"expiresAt":    expiresAt.UTC(),
		"expires_in":   int64(time.Until(expiresAt).Seconds()),
		"mfaRequired":  false,
		"mfa_required": false,
		"user":         sanitizeUser(user, nil),
	}

	if !h.isReadOnlyAccount(user) {
		if challengeToken, err := h.createMFAChallenge(user.ID); err != nil {
			slog.Error("failed to create mfa challenge during login", "err", err, "userID", user.ID)
		} else {
			response["mfaToken"] = challengeToken
		}
	}

	c.JSON(http.StatusOK, response)
}

func (h *handler) verifyMFALogin(c *gin.Context) {
	var req struct {
		MFATicket string `json:"mfa_ticket"`
		MFAToken  string `json:"mfaToken"`
		Code      string `json:"code"`
		TOTPCode  string `json:"totpCode"`
		Method    string `json:"method"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	mfaTicket := strings.TrimSpace(req.MFATicket)
	if mfaTicket == "" {
		mfaTicket = strings.TrimSpace(req.MFAToken)
	}
	if mfaTicket == "" {
		respondError(c, http.StatusBadRequest, "mfa_ticket_required", "mfa ticket is required")
		return
	}

	code := strings.TrimSpace(req.Code)
	if code == "" {
		code = strings.TrimSpace(req.TOTPCode)
	}
	if code == "" {
		respondError(c, http.StatusBadRequest, "mfa_code_required", "totp code is required")
		return
	}

	method := strings.ToLower(strings.TrimSpace(req.Method))
	if method == "" {
		method = "totp"
	}
	if method != "totp" {
		respondError(c, http.StatusBadRequest, "unsupported_mfa_method", "unsupported mfa method")
		return
	}

	challenge, ok := h.lookupMFAChallenge(mfaTicket)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_ticket", "mfa ticket is invalid or expired")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), challenge.userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "authentication_failed", "failed to authenticate user")
		return
	}
	if !user.MFAEnabled {
		respondError(c, http.StatusBadRequest, "mfa_not_enabled", "multi-factor authentication is not enabled")
		return
	}

	valid, err := totp.ValidateCustom(code, user.MFATOTPSecret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "invalid_mfa_code", "invalid totp code")
		return
	}
	if !valid {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_code", "invalid totp code")
		return
	}

	h.removeMFAChallenge(mfaTicket)

	token, expiresAt, err := h.createSession(user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
		return
	}

	h.setSessionCookie(c, token, expiresAt)
	c.JSON(http.StatusOK, gin.H{
		"message":      "login successful",
		"token":        token,
		"access_token": token,
		"expiresAt":    expiresAt.UTC(),
		"expires_in":   int64(time.Until(expiresAt).Seconds()),
		"mfaRequired":  false,
		"mfa_required": false,
		"user":         sanitizeUser(user, nil),
	})
}

type tokenRefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *handler) refreshToken(c *gin.Context) {
	if h.tokenService == nil {
		respondError(c, http.StatusServiceUnavailable, "token_service_unavailable", "token service is not configured")
		return
	}

	var req tokenRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	// Refresh access token
	accessToken, err := h.tokenService.RefreshAccessToken(req.RefreshToken)
	if err != nil {
		respondError(c, http.StatusUnauthorized, "invalid_refresh_token", "invalid or expired refresh token")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int64(h.tokenService.GetAccessTokenExpiry().Seconds()),
	})
}

type tokenExchangeRequest struct {
	ExchangeCode string `json:"exchange_code"`
}

func (h *handler) exchangeToken(c *gin.Context) {
	var req tokenExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	sessionToken, _, ok := h.consumeOAuthExchangeCode(req.ExchangeCode)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_exchange_code", "invalid or expired exchange code")
		return
	}

	sess, ok := h.lookupSession(sessionToken)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_exchange_code", "exchange session is invalid or expired")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), sess.userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_user_lookup_failed", "failed to load session user")
		return
	}

	expiresIn := int64(time.Until(sess.expiresAt).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}

	c.JSON(http.StatusOK, gin.H{
		"token":        sessionToken,
		"access_token": sessionToken,
		"token_type":   "Bearer",
		"expiresAt":    sess.expiresAt.UTC(),
		"expires_in":   expiresIn,
		"user":         sanitizeUser(user, nil),
	})
}

func (h *handler) findUserByIdentifier(ctx context.Context, identifier string) (*store.User, error) {
	user, err := h.store.GetUserByName(ctx, identifier)
	if err == nil {
		return user, nil
	}
	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return nil, err
	}
	return h.store.GetUserByEmail(ctx, identifier)
}

func (h *handler) session(c *gin.Context) {
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		if value := c.Query("token"); value != "" {
			token = value
		}
	}
	if token == "" {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			cookie = strings.TrimSpace(cookie)
			if cookie != "" {
				token = cookie
			}
		}
	}
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session token required"})
		return
	}

	sess, ok := h.lookupSession(token)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session not found"})
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), sess.userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load session user"})
		return
	}

	// Sandbox UUID rotates hourly; refresh on session reads so the UI always sees a valid UUID.
	if err := h.ensureSandboxProxyUUID(c.Request.Context(), user); err != nil {
		slog.Warn("failed to rotate sandbox proxy uuid", "err", err, "userID", user.ID)
	}

	sanitized, err := h.buildSessionUser(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantNotFound) {
			c.JSON(http.StatusOK, gin.H{"user": sanitizeUser(user, nil)})
			return
		}
		respondError(c, http.StatusInternalServerError, "session_tenant_resolution_failed", "failed to resolve tenant session context")
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": sanitized})
}

func (h *handler) deleteSession(c *gin.Context) {
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		if value := c.Query("token"); value != "" {
			token = value
		}
	}
	if token == "" {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			cookie = strings.TrimSpace(cookie)
			if cookie != "" {
				token = cookie
			}
		}
	}
	if token == "" {
		c.Status(http.StatusNoContent)
		return
	}

	h.removeSession(token)
	c.Status(http.StatusNoContent)
}

func (h *handler) requireAuthenticatedUser(c *gin.Context) (*store.User, bool) {
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		if value := c.Query("token"); value != "" {
			token = value
		}
	}
	if token == "" {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			candidate := strings.TrimSpace(cookie)
			if candidate != "" {
				token = candidate
			}
		}
	}

	if token == "" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return nil, false
	}

	sess, ok := h.lookupSession(token)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_session", "session token is invalid or expired")
		return nil, false
	}

	user, err := h.store.GetUserByID(c.Request.Context(), sess.userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_user_lookup_failed", "failed to load session user")
		return nil, false
	}

	return user, true
}

func (h *handler) createSession(userID string) (string, time.Time, error) {
	token, err := h.newRandomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	ttl := h.sessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	expiresAt := time.Now().Add(ttl)

	if err := h.store.CreateSession(context.Background(), token, userID, expiresAt); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (h *handler) setSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	secure := c.Request.TLS != nil
	c.SetSameSite(http.SameSiteLaxMode)

	domain := h.getCookieDomain()
	c.SetCookie(sessionCookieName, token, maxAge, "/", domain, secure, true)
}

func (h *handler) getCookieDomain() string {
	if h.publicURL == "" {
		return ""
	}
	u, err := url.Parse(h.publicURL)
	if err != nil {
		return ""
	}
	host := strings.Split(u.Hostname(), ":")[0]
	if host == "localhost" || host == "127.0.0.1" {
		return ""
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return "." + strings.Join(parts[len(parts)-2:], ".")
	}
	return ""
}

func (h *handler) lookupSession(token string) (session, bool) {
	userID, expiresAt, err := h.store.GetSession(context.Background(), token)
	if err != nil {
		return session{}, false
	}
	return session{userID: userID, expiresAt: expiresAt}, true
}

func (h *handler) removeSession(token string) {
	h.store.DeleteSession(context.Background(), token)
}

func (h *handler) issueOAuthExchangeCode(sessionToken string, sessionExpiresAt time.Time) (string, time.Time, error) {
	code, err := h.newRandomToken()
	if err != nil {
		return "", time.Time{}, err
	}

	expiresAt := time.Now().Add(h.oauthExchangeTTL)
	if h.oauthExchangeTTL <= 0 {
		expiresAt = time.Now().Add(defaultOAuthExchangeCodeTTL)
	}
	if !sessionExpiresAt.IsZero() && sessionExpiresAt.Before(expiresAt) {
		expiresAt = sessionExpiresAt
	}

	h.oauthExchangeMu.Lock()
	defer h.oauthExchangeMu.Unlock()
	h.oauthExchangeCodes[code] = oauthExchangeCode{
		sessionToken:     sessionToken,
		sessionExpiresAt: sessionExpiresAt,
		expiresAt:        expiresAt,
	}
	return code, expiresAt, nil
}

func (h *handler) consumeOAuthExchangeCode(code string) (string, time.Time, bool) {
	normalized := strings.TrimSpace(code)
	if normalized == "" {
		return "", time.Time{}, false
	}

	h.oauthExchangeMu.Lock()
	defer h.oauthExchangeMu.Unlock()

	record, ok := h.oauthExchangeCodes[normalized]
	if !ok {
		return "", time.Time{}, false
	}
	delete(h.oauthExchangeCodes, normalized)

	if time.Now().After(record.expiresAt) {
		return "", time.Time{}, false
	}
	return record.sessionToken, record.sessionExpiresAt, true
}

func (h *handler) newRandomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func (h *handler) newVerificationCode() (string, error) {
	max := big.NewInt(1000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (h *handler) effectiveMFAChallengeTTL() time.Duration {
	ttl := h.mfaChallengeTTL
	if ttl <= 0 {
		ttl = defaultMFAChallengeTTL
	}
	return ttl
}

func clearMFAChallenge(ch *mfaChallenge) {
	if ch == nil {
		return
	}
	ch.totpSecret = ""
	ch.totpIssuer = ""
	ch.totpAccount = ""
	ch.totpIssuedAt = time.Time{}
	ch.failedAttempts = 0
	ch.lockedUntil = time.Time{}
}

func (h *handler) updateMFAChallenge(token string, update func(*mfaChallenge) bool) (mfaChallenge, bool) {
	h.mfaMu.Lock()
	defer h.mfaMu.Unlock()

	challenge, ok := h.mfaChallenges[token]
	if !ok {
		return mfaChallenge{}, false
	}

	if time.Now().After(challenge.expiresAt) {
		clearMFAChallenge(&challenge)
		delete(h.mfaChallenges, token)
		return mfaChallenge{}, false
	}

	if update != nil {
		if !update(&challenge) {
			clearMFAChallenge(&challenge)
			delete(h.mfaChallenges, token)
			return mfaChallenge{}, false
		}
		h.mfaChallenges[token] = challenge
	}

	return challenge, true
}

func (h *handler) createMFAChallenge(userID string) (string, error) {
	token, err := h.newRandomToken()
	if err != nil {
		return "", err
	}
	ttl := h.effectiveMFAChallengeTTL()
	challenge := mfaChallenge{userID: userID, expiresAt: time.Now().Add(ttl)}
	h.mfaMu.Lock()
	h.mfaChallenges[token] = challenge
	h.mfaMu.Unlock()
	return token, nil
}

func (h *handler) lookupMFAChallenge(token string) (mfaChallenge, bool) {
	return h.updateMFAChallenge(token, nil)
}

func (h *handler) refreshMFAChallenge(token string) (mfaChallenge, bool) {
	ttl := h.effectiveMFAChallengeTTL()
	return h.updateMFAChallenge(token, func(ch *mfaChallenge) bool {
		ch.expiresAt = time.Now().Add(ttl)
		return true
	})
}

func (h *handler) enqueueEmailVerification(ctx context.Context, user *store.User) error {
	email := strings.TrimSpace(user.Email)
	if email == "" {
		return errors.New("user email is empty")
	}

	normalizedEmail := strings.ToLower(email)
	ttl := h.verificationTTL
	if ttl <= 0 {
		ttl = defaultEmailVerificationTTL
	}

	h.verificationMu.Lock()
	var code string
	var expiresAt time.Time
	if existing, ok := h.verifications[normalizedEmail]; ok && time.Now().Before(existing.expiresAt) {
		code = existing.code
		expiresAt = existing.expiresAt
	} else {
		var err error
		code, err = h.newVerificationCode()
		if err != nil {
			h.verificationMu.Unlock()
			return err
		}
		expiresAt = time.Now().Add(ttl)
		h.verifications[normalizedEmail] = emailVerification{
			userID:    user.ID,
			email:     normalizedEmail,
			code:      code,
			expiresAt: expiresAt,
		}
	}
	h.verificationMu.Unlock()

	name := strings.TrimSpace(user.Name)
	if name == "" {
		name = "there"
	}

	subject := "Verify your XControl account"
	plainBody := fmt.Sprintf("Hello %s,\n\nUse the following verification code to verify your XControl account: %s\n\nThis code expires at %s UTC (in %d minutes).\nIf you did not request this email you can ignore it.\n", name, code, expiresAt.UTC().Format(time.RFC3339), int(ttl.Minutes()))
	htmlBody := fmt.Sprintf("<p>Hello %s,</p><p>Use the following verification code to verify your XControl account:</p><p><strong>%s</strong></p><p>This code expires at %s UTC (in %d minutes).</p><p>If you did not request this email you can ignore it.</p>", html.EscapeString(name), code, expiresAt.UTC().Format(time.RFC3339), int(ttl.Minutes()))

	msg := EmailMessage{
		To:        []string{email},
		Subject:   subject,
		PlainBody: plainBody,
		HTMLBody:  htmlBody,
	}

	if err := h.emailSender.Send(ctx, msg); err != nil {
		// Log but don't delete immediately to allow retries with same code
		slog.Error("failed to send verification email", "err", err, "email", email)
		return err
	}

	return nil
}

func (h *handler) lookupEmailVerification(email string) (emailVerification, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return emailVerification{}, false
	}

	h.verificationMu.RLock()
	verification, ok := h.verifications[email]
	h.verificationMu.RUnlock()
	if !ok {
		return emailVerification{}, false
	}

	if time.Now().After(verification.expiresAt) {
		h.removeEmailVerification(email)
		return emailVerification{}, false
	}

	return verification, true
}

func (h *handler) removeEmailVerification(email string) {
	h.verificationMu.Lock()
	delete(h.verifications, strings.ToLower(strings.TrimSpace(email)))
	h.verificationMu.Unlock()
}

func (h *handler) issueRegistrationVerification(ctx context.Context, email string) (registrationVerification, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return registrationVerification{}, errors.New("email is empty")
	}

	ttl := h.verificationTTL
	if ttl <= 0 {
		ttl = defaultEmailVerificationTTL
	}

	h.registrationMu.Lock()
	var verification registrationVerification
	if existing, ok := h.registrationVerifications[normalized]; ok && time.Now().Before(existing.expiresAt) {
		verification = existing
	} else {
		code, err := h.newVerificationCode()
		if err != nil {
			h.registrationMu.Unlock()
			return registrationVerification{}, err
		}
		verification = registrationVerification{
			email:     normalized,
			code:      code,
			expiresAt: time.Now().Add(ttl),
		}
		h.registrationVerifications[normalized] = verification
	}
	h.registrationMu.Unlock()

	// [DEBUG] Log the verification code to stdout so we can see it in logs
	slog.Info("issued registration verification code", "email", normalized, "code", verification.code)

	trimmedEmail := strings.TrimSpace(email)
	if trimmedEmail == "" {
		trimmedEmail = normalized
	}

	subject := "Verify your email for XControl"
	plainBody := fmt.Sprintf(
		"Hello,\n\nUse the following verification code to continue creating your XControl account: %s\n\nThis code expires at %s UTC (in %d minutes).\nIf you did not request this email you can ignore it.\n",
		verification.code,
		verification.expiresAt.UTC().Format(time.RFC3339),
		int(ttl.Minutes()),
	)
	htmlBody := fmt.Sprintf(
		"<p>Hello,</p><p>Use the following verification code to continue creating your XControl account:</p><p><strong>%s</strong></p><p>This code expires at %s UTC (in %d minutes).</p><p>If you did not request this email you can ignore it.</p>",
		html.EscapeString(verification.code),
		verification.expiresAt.UTC().Format(time.RFC3339),
		int(ttl.Minutes()),
	)

	msg := EmailMessage{
		To:        []string{trimmedEmail},
		Subject:   subject,
		PlainBody: plainBody,
		HTMLBody:  htmlBody,
	}

	if err := h.emailSender.Send(ctx, msg); err != nil {
		// Log but don't delete to allow reuse/resend attempts
		slog.Error("failed to send registration verification email", "err", err, "email", email)
		return registrationVerification{}, err
	}

	return verification, nil
}

func (h *handler) lookupRegistrationVerification(email string) (registrationVerification, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return registrationVerification{}, false
	}

	h.registrationMu.RLock()
	verification, ok := h.registrationVerifications[email]
	h.registrationMu.RUnlock()
	if !ok {
		return registrationVerification{}, false
	}

	if time.Now().After(verification.expiresAt) {
		h.removeRegistrationVerification(email)
		return registrationVerification{}, false
	}

	return verification, true
}

func (h *handler) markRegistrationVerified(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}

	h.registrationMu.Lock()
	defer h.registrationMu.Unlock()

	verification, ok := h.registrationVerifications[email]
	if !ok {
		return false
	}

	if time.Now().After(verification.expiresAt) {
		delete(h.registrationVerifications, email)
		return false
	}

	verification.verified = true
	h.registrationVerifications[email] = verification
	return true
}

func (h *handler) removeRegistrationVerification(email string) {
	h.registrationMu.Lock()
	delete(h.registrationVerifications, strings.ToLower(strings.TrimSpace(email)))
	h.registrationMu.Unlock()
}

func (h *handler) enqueuePasswordReset(ctx context.Context, user *store.User) error {
	email := strings.TrimSpace(user.Email)
	if email == "" {
		return errors.New("user email is empty")
	}

	token, err := h.newRandomToken()
	if err != nil {
		return err
	}

	ttl := h.resetTTL
	if ttl <= 0 {
		ttl = defaultPasswordResetTTL
	}

	expiresAt := time.Now().Add(ttl)
	reset := passwordReset{
		userID:    user.ID,
		email:     strings.ToLower(email),
		expiresAt: expiresAt,
	}

	h.resetMu.Lock()
	h.passwordResets[token] = reset
	h.resetMu.Unlock()

	name := strings.TrimSpace(user.Name)
	if name == "" {
		name = "there"
	}

	subject := "Reset your XControl password"
	plainBody := fmt.Sprintf("Hello %s,\n\nUse the following token to reset your XControl account password: %s\n\nThis token expires at %s UTC.\nIf you did not request a reset you can ignore this email.\n", name, token, expiresAt.UTC().Format(time.RFC3339))
	htmlBody := fmt.Sprintf("<p>Hello %s,</p><p>Use the following token to reset your XControl account password:</p><p><strong>%s</strong></p><p>This token expires at %s UTC.</p><p>If you did not request a reset you can ignore this email.</p>", html.EscapeString(name), token, expiresAt.UTC().Format(time.RFC3339))

	msg := EmailMessage{
		To:        []string{email},
		Subject:   subject,
		PlainBody: plainBody,
		HTMLBody:  htmlBody,
	}

	if err := h.emailSender.Send(ctx, msg); err != nil {
		h.removePasswordReset(token)
		return err
	}

	return nil
}

func (h *handler) lookupPasswordReset(token string) (passwordReset, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return passwordReset{}, false
	}

	h.resetMu.RLock()
	reset, ok := h.passwordResets[token]
	h.resetMu.RUnlock()
	if !ok {
		return passwordReset{}, false
	}

	if time.Now().After(reset.expiresAt) {
		h.removePasswordReset(token)
		return passwordReset{}, false
	}

	return reset, true
}

func (h *handler) removePasswordReset(token string) {
	h.resetMu.Lock()
	delete(h.passwordResets, strings.TrimSpace(token))
	h.resetMu.Unlock()
}

func (h *handler) removeMFAChallenge(token string) {
	h.mfaMu.Lock()
	if challenge, ok := h.mfaChallenges[token]; ok {
		clearMFAChallenge(&challenge)
		delete(h.mfaChallenges, token)
	}
	h.mfaMu.Unlock()
}

func (h *handler) removeMFAChallengesForUser(userID string) {
	if userID == "" {
		return
	}
	h.mfaMu.Lock()
	for token, challenge := range h.mfaChallenges {
		if challenge.userID == userID {
			clearMFAChallenge(&challenge)
			delete(h.mfaChallenges, token)
		}
	}
	h.mfaMu.Unlock()
}

func (h *handler) provisionTOTP(c *gin.Context) {
	var req struct {
		Token   string `json:"token"`
		Issuer  string `json:"issuer"`
		Account string `json:"account"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	token := strings.TrimSpace(req.Token)
	ctx := c.Request.Context()
	var (
		user      *store.User
		err       error
		challenge mfaChallenge
		ok        bool
	)

	if token != "" {
		challenge, ok = h.refreshMFAChallenge(token)
		if !ok {
			respondError(c, http.StatusUnauthorized, "invalid_mfa_token", "mfa token is invalid or expired")
			return
		}

		user, err = h.store.GetUserByID(ctx, challenge.userID)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "mfa_user_lookup_failed", "failed to load user for mfa provisioning")
			return
		}
	} else {
		sessionToken := extractToken(c.GetHeader("Authorization"))
		if sessionToken == "" {
			respondError(c, http.StatusBadRequest, "mfa_token_required", "mfa token or valid session is required")
			return
		}

		sess, ok := h.lookupSession(sessionToken)
		if !ok {
			respondError(c, http.StatusUnauthorized, "invalid_session", "session token is invalid or expired")
			return
		}

		user, err = h.store.GetUserByID(ctx, sess.userID)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "mfa_user_lookup_failed", "failed to load user for mfa provisioning")
			return
		}

		challengeToken, err := h.createMFAChallenge(user.ID)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "mfa_challenge_creation_failed", "failed to create mfa challenge")
			return
		}

		token = challengeToken
		challenge, ok = h.refreshMFAChallenge(token)
		if !ok {
			respondError(c, http.StatusInternalServerError, "mfa_challenge_creation_failed", "failed to initialize mfa challenge")
			return
		}
	}

	if user.MFAEnabled {
		respondError(c, http.StatusBadRequest, "mfa_already_enabled", "mfa already enabled for this account")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	issuer := strings.TrimSpace(req.Issuer)
	if issuer == "" {
		issuer = strings.TrimSpace(h.totpIssuer)
		if issuer == "" {
			issuer = defaultTOTPIssuer
		}
	}

	accountName := strings.TrimSpace(req.Account)
	if accountName == "" {
		accountName = deriveDefaultAccountLabel(user, issuer)
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "mfa_secret_generation_failed", "failed to generate totp secret")
		return
	}

	issuedAt := time.Now().UTC()
	ttl := h.effectiveMFAChallengeTTL()

	pendingChallenge, ok := h.refreshMFAChallenge(token)
	if !ok || pendingChallenge.userID != user.ID {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_token", "mfa token is invalid or expired")
		return
	}

	secret := strings.TrimSpace(key.Secret())
	previousSecret := user.MFATOTPSecret
	previousIssuedAt := user.MFASecretIssuedAt
	previousConfirmedAt := user.MFAConfirmedAt
	previousEnabled := user.MFAEnabled

	user.MFATOTPSecret = secret
	user.MFASecretIssuedAt = issuedAt
	user.MFAConfirmedAt = time.Time{}
	user.MFAEnabled = false

	if err := h.store.UpdateUser(ctx, user); err != nil {
		user.MFATOTPSecret = previousSecret
		user.MFASecretIssuedAt = previousIssuedAt
		user.MFAConfirmedAt = previousConfirmedAt
		user.MFAEnabled = previousEnabled
		respondError(c, http.StatusInternalServerError, "mfa_setup_failed", "failed to persist mfa provisioning state")
		return
	}

	pendingChallenge, ok = h.updateMFAChallenge(token, func(ch *mfaChallenge) bool {
		if ch.userID != user.ID {
			return false
		}
		ch.totpSecret = secret
		ch.totpIssuer = issuer
		ch.totpAccount = accountName
		ch.totpIssuedAt = issuedAt
		ch.failedAttempts = 0
		ch.lockedUntil = time.Time{}
		ch.expiresAt = time.Now().Add(ttl)
		return true
	})
	if !ok {
		user.MFATOTPSecret = previousSecret
		user.MFASecretIssuedAt = previousIssuedAt
		user.MFAConfirmedAt = previousConfirmedAt
		user.MFAEnabled = previousEnabled
		if err := h.store.UpdateUser(ctx, user); err != nil {
			slog.Error("failed to revert mfa provisioning state", "err", err, "userID", user.ID)
		}
		respondError(c, http.StatusInternalServerError, "mfa_challenge_creation_failed", "failed to initialize mfa challenge")
		return
	}

	state := buildMFAState(user, &pendingChallenge)
	sanitized := sanitizeUser(user, &pendingChallenge)
	c.JSON(http.StatusOK, gin.H{
		"secret":      secret,
		"otpauth_url": key.URL(),
		"issuer":      issuer,
		"account":     accountName,
		"mfaToken":    token,
		"mfa":         state,
		"user":        sanitized,
	})
}

func deriveDefaultAccountLabel(user *store.User, issuer string) string {
	if user == nil {
		if issuer == "" {
			return "account"
		}
		return fmt.Sprintf("%s account", issuer)
	}

	identifier := strings.TrimSpace(user.ID)
	if identifier == "" {
		if issuer == "" {
			return "account"
		}
		return fmt.Sprintf("%s account", issuer)
	}

	sum := sha1.Sum([]byte(identifier))
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	encoded := strings.ToLower(encoder.EncodeToString(sum[:]))
	if len(encoded) > 10 {
		encoded = encoded[:10]
	}
	return fmt.Sprintf("user-%s", encoded)
}

func (h *handler) verifyTOTP(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	token := strings.TrimSpace(req.Token)
	if token == "" {
		respondError(c, http.StatusBadRequest, "mfa_token_required", "mfa token is required")
		return
	}

	challenge, ok := h.lookupMFAChallenge(token)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_token", "mfa token is invalid or expired")
		return
	}

	ctx := c.Request.Context()
	user, err := h.store.GetUserByID(ctx, challenge.userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "mfa_user_lookup_failed", "failed to load user for verification")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	challenge, ok = h.updateMFAChallenge(token, func(ch *mfaChallenge) bool {
		if ch.userID != user.ID {
			return false
		}
		ch.expiresAt = time.Now().Add(h.effectiveMFAChallengeTTL())
		return true
	})
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_mfa_token", "mfa token is invalid or expired")
		return
	}

	now := time.Now()
	if !challenge.lockedUntil.IsZero() && now.Before(challenge.lockedUntil) {
		retryAt := challenge.lockedUntil.UTC()
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":    "mfa_challenge_locked",
			"message":  "too many invalid mfa attempts, try again later",
			"retryAt":  retryAt,
			"mfaToken": token,
		})
		return
	}

	secret := strings.TrimSpace(user.MFATOTPSecret)
	if secret == "" {
		secret = strings.TrimSpace(challenge.totpSecret)
	}
	if secret == "" {
		respondError(c, http.StatusBadRequest, "mfa_secret_missing", "mfa secret has not been provisioned")
		return
	}

	code := strings.TrimSpace(req.Code)
	if code == "" {
		respondError(c, http.StatusBadRequest, "mfa_code_required", "totp code is required")
		return
	}

	valid, err := totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "invalid_mfa_code", "invalid totp code")
		return
	}
	if !valid {
		ttl := h.effectiveMFAChallengeTTL()
		updatedChallenge, ok := h.updateMFAChallenge(token, func(ch *mfaChallenge) bool {
			if ch.userID != user.ID {
				return false
			}
			if now.Before(ch.lockedUntil) {
				return true
			}
			ch.failedAttempts++
			if ch.failedAttempts >= maxMFAVerificationAttempts {
				ch.failedAttempts = 0
				ch.lockedUntil = now.Add(defaultMFALockoutDuration)
			}
			ch.expiresAt = time.Now().Add(ttl)
			return true
		})
		if ok {
			challenge = updatedChallenge
		}

		if !challenge.lockedUntil.IsZero() && now.Before(challenge.lockedUntil) {
			retryAt := challenge.lockedUntil.UTC()
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":    "mfa_challenge_locked",
				"message":  "too many invalid mfa attempts, try again later",
				"retryAt":  retryAt,
				"mfaToken": token,
			})
			return
		}

		respondError(c, http.StatusUnauthorized, "invalid_mfa_code", "invalid totp code")
		return
	}

	confirmationTime := time.Now().UTC()
	issuedAt := challenge.totpIssuedAt
	if issuedAt.IsZero() {
		issuedAt = confirmationTime
	}

	if strings.TrimSpace(user.MFATOTPSecret) == "" {
		user.MFATOTPSecret = secret
	}
	if user.MFASecretIssuedAt.IsZero() {
		user.MFASecretIssuedAt = issuedAt
	}
	user.MFAEnabled = true
	user.MFAConfirmedAt = confirmationTime

	if err := h.store.UpdateUser(ctx, user); err != nil {
		respondError(c, http.StatusInternalServerError, "mfa_update_failed", "failed to enable mfa")
		return
	}

	h.removeMFAChallenge(token)

	sessionToken, expiresAt, err := h.createSession(user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
		return
	}

	h.setSessionCookie(c, sessionToken, expiresAt)

	c.JSON(http.StatusOK, gin.H{
		"message":   "mfa_verified",
		"token":     sessionToken,
		"expiresAt": expiresAt.UTC(),
		"user":      sanitizeUser(user, nil),
	})
}

func (h *handler) mfaStatus(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		token = strings.TrimSpace(c.GetHeader("X-MFA-Token"))
	}

	identifier := strings.TrimSpace(c.Query("identifier"))
	if identifier == "" {
		identifier = strings.TrimSpace(c.Query("email"))
	}

	authToken := extractToken(c.GetHeader("Authorization"))

	var (
		user      *store.User
		err       error
		challenge *mfaChallenge
	)

	ctx := c.Request.Context()

	if authToken != "" {
		if sess, ok := h.lookupSession(authToken); ok {
			user, err = h.store.GetUserByID(ctx, sess.userID)
			if err != nil {
				respondError(c, http.StatusInternalServerError, "mfa_status_failed", "failed to load user for status")
				return
			}
		} else if token == "" {
			token = authToken
		}
	}

	if token != "" {
		if refreshed, ok := h.refreshMFAChallenge(token); ok {
			if user != nil && user.ID != refreshed.userID {
				challenge = nil
			} else {
				challenge = &refreshed
				if user == nil {
					user, err = h.store.GetUserByID(ctx, refreshed.userID)
					if err != nil {
						respondError(c, http.StatusInternalServerError, "mfa_status_failed", "failed to load user for status")
						return
					}
				}
			}
		}
	}

	if user == nil && identifier != "" {
		user, err = h.findUserByIdentifier(ctx, identifier)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				c.JSON(http.StatusOK, gin.H{
					"mfa_enabled": false,
				})
				return
			}
			respondError(c, http.StatusInternalServerError, "mfa_status_failed", "failed to load user for status")
			return
		}
	}

	if user == nil {
		respondError(c, http.StatusUnauthorized, "mfa_token_required", "valid session or mfa token is required")
		return
	}

	state := buildMFAState(user, challenge)
	c.JSON(http.StatusOK, gin.H{
		"enabled": user.MFAEnabled,
		"mfa":     state,
		"user":    sanitizeUser(user, challenge),
	})
}

func (h *handler) listSubscriptions(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	subscriptions, err := h.store.ListSubscriptionsByUser(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "subscriptions_unavailable", "failed to load subscriptions")
		return
	}

	sanitized := make([]gin.H, 0, len(subscriptions))
	for i := range subscriptions {
		sanitized = append(sanitized, sanitizeSubscription(&subscriptions[i]))
	}

	c.JSON(http.StatusOK, gin.H{"subscriptions": sanitized})
}

func (h *handler) upsertSubscription(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	var req subscriptionUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	externalID := strings.TrimSpace(req.ExternalID)
	if externalID == "" {
		respondError(c, http.StatusBadRequest, "external_id_required", "externalId is required")
		return
	}

	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = "paypal"
	}
	paymentMethod := strings.TrimSpace(req.PaymentMethod)
	if paymentMethod == "" {
		paymentMethod = provider
	}
	paymentQRCode := strings.TrimSpace(req.PaymentQRCode)
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "subscription"
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "active"
	}

	sub := &store.Subscription{
		UserID:        user.ID,
		Provider:      provider,
		PaymentMethod: paymentMethod,
		PaymentQRCode: paymentQRCode,
		Kind:          kind,
		PlanID:        strings.TrimSpace(req.PlanID),
		ExternalID:    externalID,
		Status:        status,
		Meta:          req.Meta,
	}

	if err := h.store.UpsertSubscription(c.Request.Context(), sub); err != nil {
		respondError(c, http.StatusInternalServerError, "subscription_upsert_failed", "failed to persist subscription state")
		return
	}

	c.JSON(http.StatusOK, gin.H{"subscription": sanitizeSubscription(sub)})
}

func (h *handler) cancelSubscription(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	var req subscriptionCancelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	externalID := strings.TrimSpace(req.ExternalID)
	if externalID == "" {
		respondError(c, http.StatusBadRequest, "external_id_required", "externalId is required")
		return
	}

	if h.stripe != nil && h.stripe.enabled() {
		subscriptions, err := h.store.ListSubscriptionsByUser(c.Request.Context(), user.ID)
		if err == nil {
			for i := range subscriptions {
				subscription := subscriptions[i]
				if strings.TrimSpace(subscription.ExternalID) != externalID {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(subscription.Provider), "stripe") && strings.EqualFold(strings.TrimSpace(subscription.Kind), "subscription") {
					if err := h.stripe.cancelSubscription(c.Request.Context(), externalID); err != nil {
						respondError(c, http.StatusBadGateway, "stripe_cancel_failed", "failed to cancel stripe subscription")
						return
					}
				}
				break
			}
		}
	}

	sub, err := h.store.CancelSubscription(c.Request.Context(), user.ID, externalID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrSubscriptionNotFound) {
			respondError(c, http.StatusNotFound, "subscription_not_found", "subscription not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "subscription_cancel_failed", "failed to update subscription")
		return
	}

	c.JSON(http.StatusOK, gin.H{"subscription": sanitizeSubscription(sub)})
}

func sanitizeUser(user *store.User, challenge *mfaChallenge) gin.H {
	identifier := strings.TrimSpace(user.ID)
	proxyUUID := strings.TrimSpace(user.ProxyUUID)
	if proxyUUID == "" {
		proxyUUID = identifier
	}
	groups := user.Groups
	if len(groups) == 0 {
		groups = []string{}
	} else {
		cloned := make([]string, len(groups))
		copy(cloned, groups)
		groups = cloned
	}
	permissions := user.Permissions
	if len(permissions) == 0 {
		permissions = []string{}
	} else {
		cloned := make([]string, len(permissions))
		copy(cloned, permissions)
		permissions = cloned
	}
	return gin.H{
		"id":                 identifier,
		"uuid":               identifier,
		"name":               user.Name,
		"username":           user.Name,
		"email":              user.Email,
		"emailVerified":      user.EmailVerified,
		"mfaEnabled":         user.MFAEnabled,
		"mfa":                buildMFAState(user, challenge),
		"role":               user.Role,
		"groups":             groups,
		"permissions":        permissions,
		"proxyUuid":          proxyUUID,
		"proxyUuidExpiresAt": user.ProxyUUIDExpiresAt,
	}
}

func sanitizeSubscription(sub *store.Subscription) gin.H {
	if sub == nil {
		return gin.H{}
	}

	meta := map[string]any{}
	for key, value := range sub.Meta {
		meta[key] = value
	}

	payload := gin.H{
		"id":            sub.ID,
		"userId":        sub.UserID,
		"provider":      sub.Provider,
		"paymentMethod": sub.PaymentMethod,
		"paymentQr":     strings.TrimSpace(sub.PaymentQRCode),
		"kind":          sub.Kind,
		"planId":        sub.PlanID,
		"externalId":    sub.ExternalID,
		"status":        sub.Status,
		"meta":          meta,
		"createdAt":     sub.CreatedAt.UTC(),
		"updatedAt":     sub.UpdatedAt.UTC(),
	}

	if sub.CancelledAt != nil {
		payload["cancelledAt"] = sub.CancelledAt.UTC()
	}

	return payload
}

func buildMFAState(user *store.User, challenge *mfaChallenge) gin.H {
	pending := strings.TrimSpace(user.MFATOTPSecret) != "" && !user.MFAEnabled
	issuedAt := user.MFASecretIssuedAt

	if challenge != nil && !user.MFAEnabled {
		if strings.TrimSpace(challenge.totpSecret) != "" {
			pending = true
		}
		if issuedAt.IsZero() && !challenge.totpIssuedAt.IsZero() {
			issuedAt = challenge.totpIssuedAt
		}
	}

	state := gin.H{
		"totpEnabled": user.MFAEnabled,
		"totpPending": pending,
	}
	if !issuedAt.IsZero() {
		state["totpSecretIssuedAt"] = issuedAt.UTC()
	}
	if !user.MFAConfirmedAt.IsZero() {
		state["totpConfirmedAt"] = user.MFAConfirmedAt.UTC()
	}
	if challenge != nil && !challenge.lockedUntil.IsZero() && time.Now().Before(challenge.lockedUntil) {
		state["totpLockedUntil"] = challenge.lockedUntil.UTC()
	}
	return state
}

func (h *handler) disableMFA(c *gin.Context) {
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		token = strings.TrimSpace(c.Query("token"))
	}
	if token == "" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return
	}

	sess, ok := h.lookupSession(token)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_session", "session token is invalid or expired")
		return
	}

	ctx := c.Request.Context()
	user, err := h.store.GetUserByID(ctx, sess.userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "mfa_disable_failed", "failed to load user for mfa disable")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	hasSecret := strings.TrimSpace(user.MFATOTPSecret) != ""
	if !user.MFAEnabled && !hasSecret {
		respondError(c, http.StatusBadRequest, "mfa_not_enabled", "multi-factor authentication is not enabled")
		return
	}

	user.MFATOTPSecret = ""
	user.MFAEnabled = false
	user.MFASecretIssuedAt = time.Time{}
	user.MFAConfirmedAt = time.Time{}

	if err := h.store.UpdateUser(ctx, user); err != nil {
		respondError(c, http.StatusInternalServerError, "mfa_disable_failed", "failed to disable mfa")
		return
	}

	h.removeMFAChallengesForUser(user.ID)

	c.JSON(http.StatusOK, gin.H{
		"message": "mfa_disabled",
		"user":    sanitizeUser(user, nil),
	})
}

func (h *handler) oauthLogin(c *gin.Context) {
	providerName := c.Param("provider")
	provider, ok := h.oauthProviders[providerName]
	if !ok {
		respondError(c, http.StatusNotFound, "provider_not_found", "oauth provider not found")
		return
	}

	state := buildOAuthState(h.resolveFrontendURL(c))
	// In a real app, we should store state in a secure cookie or session.
	// For now, we'll just redirect.
	c.Redirect(http.StatusTemporaryRedirect, provider.AuthCodeURL(state))
}

func (h *handler) oauthCallback(c *gin.Context) {
	providerName := c.Param("provider")
	provider, ok := h.oauthProviders[providerName]
	if !ok {
		respondError(c, http.StatusNotFound, "provider_not_found", "oauth provider not found")
		return
	}

	code := c.Query("code")
	if code == "" {
		respondError(c, http.StatusBadRequest, "code_missing", "oauth code missing")
		return
	}

	token, err := provider.Exchange(c.Request.Context(), code)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "oauth_exchange_failed", "failed to exchange oauth code")
		return
	}

	profile, err := provider.FetchProfile(c.Request.Context(), token)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "fetch_profile_failed", "failed to fetch user profile")
		return
	}

	if profile.Email == "" {
		respondError(c, http.StatusBadRequest, "email_missing", "email not provided by oauth provider")
		return
	}

	if !profile.Verified {
		respondError(c, http.StatusUnauthorized, "email_not_verified", "oauth email must be verified")
		return
	}

	var user *store.User
	ctx := c.Request.Context()
	existingUser, err := h.store.GetUserByEmail(ctx, profile.Email)
	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		respondError(c, http.StatusInternalServerError, "store_error", "database error")
		return
	}

	if errors.Is(err, store.ErrUserNotFound) {
		// Auto-register user
		user = &store.User{
			Name:          profile.Name,
			Email:         profile.Email,
			EmailVerified: true, // Trusted provider, verified above
			Level:         store.LevelUser,
			Role:          store.RoleUser,
			Groups:        []string{"User"},
			Active:        true,
		}
		if err := h.store.CreateUser(ctx, user); err != nil {
			respondError(c, http.StatusInternalServerError, "user_creation_failed", "failed to create user")
			return
		}

		// Provision trial
		trialExpiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
		trial := &store.Subscription{
			UserID:        user.ID,
			Provider:      "trial",
			PaymentMethod: "trial",
			Kind:          "trial",
			PlanID:        "TRIAL-7D",
			ExternalID:    fmt.Sprintf("trial-%s", user.ID),
			Status:        "active",
			Meta:          map[string]any{"expiresAt": trialExpiresAt},
		}
		h.store.UpsertSubscription(ctx, trial)
	} else {
		user = existingUser
		// Ensure user is verified if they logged in via OAuth
		if !user.EmailVerified {
			user.EmailVerified = true
			if err := h.store.UpdateUser(ctx, user); err != nil {
				slog.Warn("failed to update user verification status during oauth", "err", err, "userID", user.ID)
			}
		}
	}

	// Always ensure identity record exists (bind OAuth ID to User Email)
	identity := &store.Identity{
		UserID:     user.ID,
		Provider:   providerName,
		ExternalID: profile.ID,
	}
	if err := h.store.CreateIdentity(ctx, identity); err != nil {
		// Only log error if it's not a "already exists" error
		if !strings.Contains(err.Error(), "exists") {
			slog.Warn("failed to create identity record during oauth binding", "err", err, "userID", user.ID)
		}
	}

	sessionToken, sessionExpiresAt, err := h.createSession(user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_creation_failed", "failed to create session")
		return
	}

	exchangeCode, _, err := h.issueOAuthExchangeCode(sessionToken, sessionExpiresAt)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "exchange_code_creation_failed", "failed to issue exchange code")
		return
	}

	frontendURL := h.validateFrontendURL(parseOAuthStateFrontendURL(c.Query("state")))
	if frontendURL == "" {
		frontendURL = h.resolveFrontendURL(c)
	}
	if frontendURL == "" {
		frontendURL = h.oauthFrontendURL
	}
	if frontendURL == "" {
		frontendURL = "http://localhost:3000"
	}
	targetURL := fmt.Sprintf("%s/login?exchange_code=%s",
		strings.TrimSuffix(frontendURL, "/"),
		url.QueryEscape(exchangeCode))
	c.Redirect(http.StatusTemporaryRedirect, targetURL)
}

func (h *handler) listUsers(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersListRead); !ok {
		return
	}

	users, err := h.store.ListUsers(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "list_users_failed", "failed to fetch users")
		return
	}

	sanitized := make([]gin.H, 0, len(users))
	for _, u := range users {
		sanitized = append(sanitized, sanitizeUser(&u, nil))
	}

	c.JSON(http.StatusOK, sanitized)
}

func (h *handler) updateUserRole(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite); !ok {
		return
	}

	userId := c.Param("userId")
	if userId == "" {
		respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	role := strings.ToLower(strings.TrimSpace(req.Role))
	if _, ok := assignableUserRoles[role]; !ok {
		respondError(c, http.StatusBadRequest, "invalid_role", "specified role is not allowed")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), userId)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to fetch user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account role cannot be modified")
		return
	}

	user.Role = role
	// Role field update will trigger Level update in store if implemented according to plan
	// In store.go, normalizeUserRoleFields handles it.
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to update user")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "role updated", "user": sanitizeUser(user, nil)})
}

func (h *handler) resetUserRole(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite); !ok {
		return
	}

	userId := c.Param("userId")
	if userId == "" {
		respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), userId)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to fetch user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account role cannot be modified")
		return
	}

	user.Role = store.RoleUser
	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to update user")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "role reset", "user": sanitizeUser(user, nil)})
}

func (h *handler) generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *handler) isReadOnlyAccount(user *store.User) bool {
	if user == nil {
		return false
	}
	// Hardcoded whitelist for admin@svc.plus to bypass read-only checks if they have admin role
	email := strings.ToLower(strings.TrimSpace(user.Email))
	if email == "admin@svc.plus" {
		return false
	}

	// Root/SuperAdmin is never read-only (unless we want to enforce it for everyone else)
	if isRootUser(user) {
		return false
	}

	// Explicit Read-Only Roles/Groups
	if strings.EqualFold(strings.TrimSpace(user.Role), store.RoleReadOnly) {
		return true
	}
	for _, group := range user.Groups {
		if strings.EqualFold(strings.TrimSpace(group), "ReadOnly Role") {
			return true
		}
	}

	// Standard Sandbox users are always read-only
	name := strings.TrimSpace(user.Name)
	if strings.EqualFold(name, "sandbox") ||
		strings.EqualFold(email, sandboxUserEmail) {
		return true
	}

	// Default policy: Allow modification for regular users.
	// We only restrict explicitly flagged "demo" or "sandbox" identities or users assigned to a specific "ReadOnly Role".
	return false
}

func isRootUser(user *store.User) bool {
	// Use store.LevelAdmin as the threshold since LevelSuperAdmin is not defined
	// and RoleRoot/RoleAdmin identify admin privileges.
	return user.Level <= store.LevelAdmin || strings.EqualFold(user.Role, store.RoleRoot) || strings.EqualFold(user.Role, store.RoleAdmin)
}

func (h *handler) isRootAccount(user *store.User) bool {
	if user == nil {
		return false
	}
	return store.IsRootRole(user.Role) && strings.EqualFold(strings.TrimSpace(user.Email), store.RootAdminEmail)
}

func parseImageVersionInfo(imageRef string) imageVersionInfo {
	ref := strings.TrimSpace(imageRef)
	info := imageVersionInfo{ImageRef: ref}
	if ref == "" {
		return info
	}

	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	}

	tag := ref
	if idx := strings.LastIndex(tag, ":"); idx >= 0 && idx > strings.LastIndex(tag, "/") {
		tag = tag[idx+1:]
	}
	tag = strings.TrimSpace(tag)
	info.Tag = tag

	switch {
	case isHexCommit(tag):
		info.Commit = tag
		info.Version = tag
	case strings.HasPrefix(tag, "sha-") && isHexCommit(strings.TrimPrefix(tag, "sha-")):
		info.Commit = strings.TrimPrefix(tag, "sha-")
		info.Version = tag
	case strings.HasPrefix(tag, "v") && len(tag) > 1:
		info.Version = tag
	default:
		info.Version = tag
	}

	return info
}

func isHexCommit(value string) bool {
	if len(value) < 7 || len(value) > 40 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func respondError(c *gin.Context, status int, code, message string) {
	if status >= 500 {
		slog.Error("api_error", "status", status, "code", code, "message", message, "path", c.Request.URL.Path, "method", c.Request.Method)
	}

	c.JSON(status, gin.H{
		"error":   code,
		"message": message,
	})
}

func extractToken(header string) string {
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if strings.HasPrefix(header, prefix) {
		header = header[len(prefix):]
	}
	return strings.TrimSpace(header)
}
