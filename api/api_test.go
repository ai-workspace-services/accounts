package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"

	"account/internal/agentserver"
	"account/internal/auth"
	"account/internal/service"
	"account/internal/store"
)

type apiResponse struct {
	Message   string                 `json:"message"`
	Error     string                 `json:"error"`
	Token     string                 `json:"token"`
	MFAToken  string                 `json:"mfaToken"`
	User      map[string]interface{} `json:"user"`
	MFA       map[string]interface{} `json:"mfa"`
	Secret    string                 `json:"secret"`
	Otpauth   string                 `json:"otpauth_url"`
	ExpiresAt string                 `json:"expiresAt"`
}

type syncConfigResponse struct {
	Changed      bool                     `json:"changed"`
	Version      int64                    `json:"version"`
	RenderedJSON string                   `json:"rendered_json"`
	Digest       string                   `json:"digest"`
	Warnings     []string                 `json:"warnings"`
	Nodes        []map[string]interface{} `json:"nodes"`
	Meta         struct {
		Digest   string   `json:"digest"`
		Warnings []string `json:"warnings"`
	} `json:"meta"`
}

type capturedEmail struct {
	To        []string
	Subject   string
	PlainBody string
	HTMLBody  string
}

type stubMetricsProvider struct {
	metrics service.UserMetrics
	err     error
	called  *bool
}

func (s *stubMetricsProvider) Compute(context.Context) (service.UserMetrics, error) {
	if s.called != nil {
		*s.called = true
	}
	if s.err != nil {
		return service.UserMetrics{}, s.err
	}
	return s.metrics, nil
}

type stubOAuthProvider struct {
	profile     *auth.OAuthUserProfile
	exchangeErr error
	profileErr  error
}

func (s *stubOAuthProvider) AuthCodeURL(state string) string {
	return "https://oauth.example.test/authorize?state=" + state
}

func (s *stubOAuthProvider) Exchange(context.Context, string) (*oauth2.Token, error) {
	if s.exchangeErr != nil {
		return nil, s.exchangeErr
	}
	return &oauth2.Token{AccessToken: "oauth-token", TokenType: "Bearer"}, nil
}

func (s *stubOAuthProvider) FetchProfile(context.Context, *oauth2.Token) (*auth.OAuthUserProfile, error) {
	if s.profileErr != nil {
		return nil, s.profileErr
	}
	if s.profile == nil {
		return nil, errors.New("missing oauth profile")
	}
	cloned := *s.profile
	return &cloned, nil
}

func (s *stubOAuthProvider) Name() string {
	return "github"
}

type testEmailSender struct {
	mu       sync.Mutex
	messages []capturedEmail
}

type stubAgentStatusReader struct {
	statuses []agentserver.StatusSnapshot
}

func (s stubAgentStatusReader) Statuses() []agentserver.StatusSnapshot {
	return append([]agentserver.StatusSnapshot(nil), s.statuses...)
}

func (s *testEmailSender) Send(ctx context.Context, msg EmailMessage) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	copyTo := make([]string, len(msg.To))
	copy(copyTo, msg.To)
	s.messages = append(s.messages, capturedEmail{
		To:        copyTo,
		Subject:   msg.Subject,
		PlainBody: msg.PlainBody,
		HTMLBody:  msg.HTMLBody,
	})
	return nil
}

func (s *testEmailSender) last() (capturedEmail, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return capturedEmail{}, false
	}
	return s.messages[len(s.messages)-1], true
}

func extractTokenFromMessage(t *testing.T, msg capturedEmail) string {
	t.Helper()
	re := regexp.MustCompile(`[a-f0-9]{64}`)
	if match := re.FindString(msg.PlainBody); match != "" {
		return match
	}
	if match := re.FindString(msg.HTMLBody); match != "" {
		return match
	}
	t.Fatalf("failed to extract token from email body: %q", msg.PlainBody)
	return ""
}

func extractVerificationCodeFromMessage(t *testing.T, msg capturedEmail) string {
	t.Helper()
	re := regexp.MustCompile(`\b[0-9]{6}\b`)
	if match := re.FindString(msg.PlainBody); match != "" {
		return match
	}
	if match := re.FindString(msg.HTMLBody); match != "" {
		return match
	}
	t.Fatalf("failed to extract verification code from email body: %q", msg.PlainBody)
	return ""
}

func TestWithAgentRegistry_IgnoresTypedNil(t *testing.T) {
	var registry *agentserver.Registry
	h := &handler{}

	WithAgentRegistry(registry)(h)

	if h.agentRegistry != nil {
		t.Fatalf("expected nil agent registry, got %T", h.agentRegistry)
	}
}

func decodeResponse(t *testing.T, rr *httptest.ResponseRecorder) apiResponse {
	t.Helper()
	var resp apiResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

func newAuthenticatedSyncHarness(t *testing.T, opts ...Option) (*gin.Engine, *store.User, string) {
	t.Helper()

	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{
		Name:          "Sync User",
		Email:         "sync@example.com",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create sync user: %v", err)
	}

	token := "sync-session-token"
	if err := st.CreateSession(ctx, token, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create sync session: %v", err)
	}

	freshUser, err := st.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload sync user: %v", err)
	}

	router := gin.New()
	baseOpts := []Option{
		WithStore(st),
		WithEmailVerification(false),
	}
	RegisterRoutes(router, append(baseOpts, opts...)...)
	return router, freshUser, token
}

func decodeSyncConfigResponse(t *testing.T, rr *httptest.ResponseRecorder) syncConfigResponse {
	t.Helper()
	var resp syncConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode sync response: %v", err)
	}
	return resp
}

func TestAgentServerUsers_DefaultSyncIncludesSandboxAndRegularUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	st := store.NewMemoryStore()

	// sandbox user (special rotating proxy uuid)
	if err := st.CreateUser(ctx, &store.User{
		Name:          "Sandbox",
		Email:         "sandbox@svc.plus",
		EmailVerified: true,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}); err != nil {
		t.Fatalf("create sandbox user: %v", err)
	}

	// normal user (not verified + expired) should still be synced by default.
	if err := st.CreateUser(ctx, &store.User{
		Name:          "User",
		Email:         "user@example.com",
		EmailVerified: false,
		Role:          store.RoleUser,
		Level:         store.LevelUser,
		Active:        true,
	}); err != nil {
		t.Fatalf("create normal user: %v", err)
	}

	// Ensure normal user is "expired" per proxy UUID expiry metadata.
	sandbox, err := st.GetUserByEmail(ctx, "sandbox@svc.plus")
	if err != nil {
		t.Fatalf("get sandbox user: %v", err)
	}
	normal, err := st.GetUserByEmail(ctx, "user@example.com")
	if err != nil {
		t.Fatalf("get normal user: %v", err)
	}
	exp := time.Now().UTC().Add(-24 * time.Hour)
	normal.ProxyUUIDExpiresAt = &exp
	if err := st.UpdateUser(ctx, normal); err != nil {
		t.Fatalf("update normal user: %v", err)
	}

	registry, err := agentserver.NewRegistry(agentserver.Config{
		Credentials: []agentserver.Credential{{
			ID:     "*",
			Name:   "test-agent",
			Token:  "agent-token",
			Groups: []string{"internal"},
		}},
	})
	if err != nil {
		t.Fatalf("new agent registry: %v", err)
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithEmailVerification(false))

	req := httptest.NewRequest(http.MethodGet, "/api/agent-server/v1/users", nil)
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("X-Agent-ID", "hk-xhttp.svc.plus")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Clients []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	seenSandbox := false
	seenNormal := false
	for _, c := range payload.Clients {
		if c.Email == strings.ToLower(strings.TrimSpace(sandbox.Email)) && strings.TrimSpace(c.ID) != "" {
			seenSandbox = true
		}
		if c.Email == strings.ToLower(strings.TrimSpace(normal.Email)) && strings.TrimSpace(c.ID) != "" {
			seenNormal = true
		}
	}

	if !seenSandbox {
		t.Fatalf("expected sandbox client in response, got=%v", payload.Clients)
	}
	if !seenNormal {
		t.Fatalf("expected normal client in response, got=%v", payload.Clients)
	}
}

func waitForStableTOTPWindow(t *testing.T) {
	t.Helper()
	const period int64 = 30
	remainder := time.Now().Unix() % period
	const buffer int64 = 10
	if remainder > period-buffer {
		sleep := (period - remainder) + 2
		if sleep > 0 {
			time.Sleep(time.Duration(sleep) * time.Second)
		}
	}
}

func TestRegisterEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	mailer := &testEmailSender{}
	RegisterRoutes(router, WithEmailSender(mailer))

	email := "user@example.com"

	sendPayload := map[string]string{"email": email}
	sendBody, err := json.Marshal(sendPayload)
	if err != nil {
		t.Fatalf("failed to marshal send payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(sendBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification send success, got %d: %s", rr.Code, rr.Body.String())
	}

	msg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email to be sent")
	}
	code := extractVerificationCodeFromMessage(t, msg)

	verifyPayload := map[string]string{"email": email, "code": code}
	verifyBody, err := json.Marshal(verifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verify payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success, got %d: %s", rr.Code, rr.Body.String())
	}

	registerPayload := map[string]string{
		"name":     "Test User",
		"email":    email,
		"password": "supersecure",
		"code":     code,
	}
	registerBody, err := json.Marshal(registerPayload)
	if err != nil {
		t.Fatalf("failed to marshal register payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected registration success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.User == nil {
		t.Fatalf("expected user object in response")
	}

	if verified, ok := resp.User["emailVerified"].(bool); !ok || !verified {
		t.Fatalf("expected emailVerified true after registration, got %#v", resp.User["emailVerified"])
	}

	if emailValue, ok := resp.User["email"].(string); !ok || emailValue != email {
		t.Fatalf("expected email %q, got %#v", email, resp.User["email"])
	}

	if id, ok := resp.User["id"].(string); !ok || id == "" {
		t.Fatalf("expected user id in response")
	} else if uuid, ok := resp.User["uuid"].(string); !ok || uuid != id {
		t.Fatalf("expected uuid to match id")
	}

	if role, ok := resp.User["role"].(string); !ok || role != store.RoleUser {
		t.Fatalf("expected role %q, got %#v", store.RoleUser, resp.User["role"])
	}

	groups, ok := resp.User["groups"].([]interface{})
	if !ok || len(groups) == 0 {
		t.Fatalf("expected groups array in response")
	}
}

func TestOAuthCallbackIssuesOneTimeExchangeCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	profile := &auth.OAuthUserProfile{
		ID:       "oauth-user-1",
		Email:    "oauth-user@example.com",
		Name:     "OAuth User",
		Verified: true,
	}
	RegisterRoutes(
		router,
		WithStore(store.NewMemoryStore()),
		WithOAuthProviders(map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{profile: profile},
		}),
		WithOAuthFrontendURL("https://console.svc.plus"),
	)

	callbackReq := httptest.NewRequest(http.MethodGet, "/api/auth/oauth/callback/github?code=test-oauth-code", nil)
	callbackRec := httptest.NewRecorder()
	router.ServeHTTP(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected oauth callback redirect, got %d: %s", callbackRec.Code, callbackRec.Body.String())
	}

	location := callbackRec.Header().Get("Location")
	if location == "" {
		t.Fatalf("expected oauth callback to set redirect location")
	}
	redirectURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect url: %v", err)
	}
	if redirectURL.Query().Get("public_token") != "" {
		t.Fatalf("expected public_token to be removed from oauth redirect, got %q", location)
	}
	if redirectURL.Query().Get("userId") != "" || redirectURL.Query().Get("role") != "" {
		t.Fatalf("expected redirect to avoid caller-asserted identity fields, got %q", location)
	}

	exchangeCode := redirectURL.Query().Get("exchange_code")
	if exchangeCode == "" {
		t.Fatalf("expected oauth redirect to include exchange_code, got %q", location)
	}

	exchangeBody, err := json.Marshal(map[string]string{"exchange_code": exchangeCode})
	if err != nil {
		t.Fatalf("marshal exchange payload: %v", err)
	}

	exchangeReq := httptest.NewRequest(http.MethodPost, "/api/auth/token/exchange", bytes.NewReader(exchangeBody))
	exchangeReq.Header.Set("Content-Type", "application/json")
	exchangeRec := httptest.NewRecorder()
	router.ServeHTTP(exchangeRec, exchangeReq)

	if exchangeRec.Code != http.StatusOK {
		t.Fatalf("expected successful token exchange, got %d: %s", exchangeRec.Code, exchangeRec.Body.String())
	}

	var exchangeResp struct {
		Token       string                 `json:"token"`
		AccessToken string                 `json:"access_token"`
		User        map[string]interface{} `json:"user"`
	}
	if err := json.Unmarshal(exchangeRec.Body.Bytes(), &exchangeResp); err != nil {
		t.Fatalf("decode exchange response: %v", err)
	}
	if exchangeResp.Token == "" {
		t.Fatalf("expected exchanged session token")
	}
	if exchangeResp.AccessToken != exchangeResp.Token {
		t.Fatalf("expected access_token alias to match session token")
	}
	if exchangeResp.User == nil {
		t.Fatalf("expected exchange response user payload")
	}
	if got := exchangeResp.User["email"]; got != profile.Email {
		t.Fatalf("expected exchange response email %q, got %#v", profile.Email, got)
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessionReq.Header.Set("Authorization", "Bearer "+exchangeResp.Token)
	sessionRec := httptest.NewRecorder()
	router.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusOK {
		t.Fatalf("expected exchanged session token to resolve session, got %d: %s", sessionRec.Code, sessionRec.Body.String())
	}

	replayReq := httptest.NewRequest(http.MethodPost, "/api/auth/token/exchange", bytes.NewReader(exchangeBody))
	replayReq.Header.Set("Content-Type", "application/json")
	replayRec := httptest.NewRecorder()
	router.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected single-use exchange code replay to fail, got %d: %s", replayRec.Code, replayRec.Body.String())
	}

	var replayResp apiResponse
	if err := json.Unmarshal(replayRec.Body.Bytes(), &replayResp); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if replayResp.Error != "invalid_exchange_code" {
		t.Fatalf("expected invalid_exchange_code on replay, got %#v", replayResp.Error)
	}
}

func TestSyncConfigSnapshotReturnsRenderedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router, user, token := newAuthenticatedSyncHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sync/config?since_version=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected sync config success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeSyncConfigResponse(t, rr)
	if !resp.Changed {
		t.Fatalf("expected changed=true for initial sync")
	}
	if resp.Version != deriveSyncVersion(user) {
		t.Fatalf("expected sync version %d, got %d", deriveSyncVersion(user), resp.Version)
	}
	if strings.TrimSpace(resp.RenderedJSON) == "" {
		t.Fatalf("expected rendered_json to be returned")
	}
	if len(resp.Nodes) == 0 {
		t.Fatalf("expected sync response to include nodes")
	}
	if strings.TrimSpace(resp.Digest) == "" {
		t.Fatalf("expected digest to be populated")
	}
	if resp.Meta.Digest != resp.Digest {
		t.Fatalf("expected top-level digest and meta digest to match, got %q and %q", resp.Digest, resp.Meta.Digest)
	}
	if len(resp.Warnings) != 0 || len(resp.Meta.Warnings) != 0 {
		t.Fatalf("expected no warnings, got top=%v meta=%v", resp.Warnings, resp.Meta.Warnings)
	}
}

func TestSyncConfigSnapshotSkipsRenderingWhenVersionUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	renderCalls := 0
	router, user, token := newAuthenticatedSyncHarness(t, WithXrayConfigRenderer(func(*store.User) (string, string, []string, error) {
		renderCalls++
		return `{"outbounds":[{"tag":"proxy","protocol":"vless"}]}`, "digest", nil, nil
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/auth/sync/config?since_version="+strconv.FormatInt(deriveSyncVersion(user), 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected unchanged sync config success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeSyncConfigResponse(t, rr)
	if resp.Changed {
		t.Fatalf("expected changed=false when since_version matches current version")
	}
	if renderCalls != 0 {
		t.Fatalf("expected renderer to be skipped when config version is unchanged, got %d call(s)", renderCalls)
	}
	if strings.TrimSpace(resp.RenderedJSON) != "" {
		t.Fatalf("expected no rendered_json when sync payload is unchanged, got %q", resp.RenderedJSON)
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("expected unchanged sync response to omit nodes, got %d", len(resp.Nodes))
	}
}

func TestSyncConfigSnapshotFallsBackWhenRenderFails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router, _, token := newAuthenticatedSyncHarness(t, WithXrayConfigRenderer(func(*store.User) (string, string, []string, error) {
		return "", "", nil, errors.New("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/auth/sync/config?since_version=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected sync config to degrade gracefully, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeSyncConfigResponse(t, rr)
	if !resp.Changed {
		t.Fatalf("expected changed=true for fallback sync payload")
	}
	if strings.TrimSpace(resp.RenderedJSON) != "" {
		t.Fatalf("expected rendered_json to be omitted on render failure, got %q", resp.RenderedJSON)
	}
	if len(resp.Nodes) == 0 {
		t.Fatalf("expected fallback sync response to include nodes")
	}
	if len(resp.Meta.Warnings) == 0 {
		t.Fatalf("expected fallback warning, got none")
	}
	if got := strings.TrimSpace(resp.Meta.Warnings[0]); !strings.Contains(got, "falling back to node metadata") {
		t.Fatalf("expected fallback warning, got %v", resp.Meta.Warnings)
	}
	if len(resp.Warnings) != len(resp.Meta.Warnings) || resp.Warnings[0] != resp.Meta.Warnings[0] {
		t.Fatalf("expected top-level warnings to mirror meta warnings, got top=%v meta=%v", resp.Warnings, resp.Meta.Warnings)
	}
	if vlessURI, ok := resp.Nodes[0]["vless_uri"].(string); !ok || strings.TrimSpace(vlessURI) == "" {
		t.Fatalf("expected fallback node payload to include vless_uri, got %v", resp.Nodes[0]["vless_uri"])
	}
}

func TestSyncConfigSnapshotIncludesNodeDisplayMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router, _, token := newAuthenticatedSyncHarness(t, WithAgentStatusReader(stubAgentStatusReader{
		statuses: []agentserver.StatusSnapshot{
			{Agent: agentserver.Identity{ID: "jp-xhttp.svc.plus", Name: "Japan Node"}},
			{Agent: agentserver.Identity{ID: "us-xhttp.svc.plus", Name: "US Node"}},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/auth/sync/config?since_version=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected sync config success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeSyncConfigResponse(t, rr)
	if len(resp.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(resp.Nodes))
	}

	byHost := make(map[string]map[string]interface{}, len(resp.Nodes))
	for _, node := range resp.Nodes {
		host, _ := node["host"].(string)
		if strings.TrimSpace(host) == "" {
			t.Fatalf("expected node host to be populated: %#v", node)
		}
		byHost[host] = node
	}

	jp := byHost["jp-xhttp.svc.plus"]
	if jp == nil {
		t.Fatalf("expected jp-xhttp.svc.plus node in response: %#v", byHost)
	}
	if got, _ := jp["id"].(string); got != "jp-xhttp.svc.plus" {
		t.Fatalf("expected node id to match host, got %q", got)
	}
	if got, _ := jp["name"].(string); got != "Japan Node" {
		t.Fatalf("expected node name to preserve display name, got %q", got)
	}
	if got, _ := jp["display_name"].(string); got != "Japan Node" {
		t.Fatalf("expected display_name to preserve display name, got %q", got)
	}
	if got, _ := jp["server_name"].(string); got != "jp-xhttp.svc.plus" {
		t.Fatalf("expected server_name to match host, got %q", got)
	}
	if got, _ := jp["uri_scheme_xhttp"].(string); strings.TrimSpace(got) == "" {
		t.Fatalf("expected uri_scheme_xhttp to be populated")
	}
}

func TestSyncConfigAckReturnsReceipt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router, user, token := newAuthenticatedSyncHarness(t)
	body := bytes.NewBufferString(`{
		"version": 123,
		"device_id": "deadbeef",
		"applied_at": "2026-03-20T12:00:00Z"
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/sync/ack", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected sync ack success, got %d: %s", rr.Code, rr.Body.String())
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode ack payload: %v", err)
	}
	if acked, _ := payload["acked"].(bool); !acked {
		t.Fatalf("expected acked=true, got %#v", payload["acked"])
	}
	if got, _ := payload["device_id"].(string); got != "deadbeef" {
		t.Fatalf("expected device_id to round-trip, got %q", got)
	}
	if got, _ := payload["user_id"].(string); got != user.ID {
		t.Fatalf("expected user_id %q, got %q", user.ID, got)
	}
}

func TestResendVerificationEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	mailer := &testEmailSender{}
	RegisterRoutes(router, WithEmailSender(mailer))

	email := "resend@example.com"

	sendPayload := map[string]string{"email": email}
	sendBody, err := json.Marshal(sendPayload)
	if err != nil {
		t.Fatalf("failed to marshal send payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(sendBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected initial send success, got %d: %s", rr.Code, rr.Body.String())
	}

	initialMsg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email after initial send")
	}
	initialCode := extractVerificationCodeFromMessage(t, initialMsg)

	resendPayload := map[string]string{"email": email}
	resendBody, err := json.Marshal(resendPayload)
	if err != nil {
		t.Fatalf("failed to marshal resend payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(resendBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected resend success, got %d: %s", rr.Code, rr.Body.String())
	}

	resentMsg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email after resend")
	}
	resentCode := extractVerificationCodeFromMessage(t, resentMsg)
	if strings.TrimSpace(resentCode) == "" {
		t.Fatalf("expected verification code in resent email")
	}
	if strings.TrimSpace(initialCode) == strings.TrimSpace(resentCode) {
		t.Logf("verification code repeated across resend; continuing to verify")
	}

	verifyPayload := map[string]string{
		"email": email,
		"code":  resentCode,
	}
	verifyBody, err := json.Marshal(verifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verify payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success after resend, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResendVerificationEndpointErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	mailer := &testEmailSender{}
	RegisterRoutes(router, WithEmailSender(mailer))

	email := "verified@example.com"

	sendPayload := map[string]string{"email": email}
	sendBody, err := json.Marshal(sendPayload)
	if err != nil {
		t.Fatalf("failed to marshal send payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(sendBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected initial send success, got %d: %s", rr.Code, rr.Body.String())
	}

	msg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email after send")
	}
	code := extractVerificationCodeFromMessage(t, msg)

	verifyPayload := map[string]string{"email": email, "code": code}
	verifyBody, err := json.Marshal(verifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verify payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success, got %d: %s", rr.Code, rr.Body.String())
	}

	registerPayload := map[string]string{
		"name":     "Verified User",
		"email":    email,
		"password": "supersecure",
		"code":     code,
	}
	registerBody, err := json.Marshal(registerPayload)
	if err != nil {
		t.Fatalf("failed to marshal register payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected registration success, got %d: %s", rr.Code, rr.Body.String())
	}

	resendPayload := map[string]string{"email": email}
	resendBody, err := json.Marshal(resendPayload)
	if err != nil {
		t.Fatalf("failed to marshal resend payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(resendBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected resend to fail for verified email, got %d: %s", rr.Code, rr.Body.String())
	}

	invalidPayload := map[string]string{"email": ""}
	invalidBody, err := json.Marshal(invalidPayload)
	if err != nil {
		t.Fatalf("failed to marshal invalid payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(invalidBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected resend to fail for invalid email, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterEndpointWithoutEmailVerification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router, WithEmailVerification(false))

	payload := map[string]string{
		"name":     "Another User",
		"email":    "another@example.com",
		"password": "supersecure",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d, body: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Message != "registration successful" {
		t.Fatalf("expected success message when verification disabled, got %q", resp.Message)
	}

	if resp.User == nil {
		t.Fatalf("expected user object in response")
	}

	if verified, ok := resp.User["emailVerified"].(bool); !ok || !verified {
		t.Fatalf("expected emailVerified true when verification disabled, got %#v", resp.User["emailVerified"])
	}
}

func TestRegisterSendEndpointWithoutEmailVerification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router, WithEmailVerification(false))

	payload := map[string]string{"email": "disabled@example.com"}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Message != "verification email sent" {
		t.Fatalf("expected verification success message, got %q", resp.Message)
	}
}

func TestSessionEndpointAcceptsCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router, WithEmailVerification(false))

	registerPayload := map[string]string{
		"name":     "Cookie User",
		"email":    "cookie-user@example.com",
		"password": "supersecure",
	}
	registerBody, err := json.Marshal(registerPayload)
	if err != nil {
		t.Fatalf("failed to marshal registration payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected registration success, got %d: %s", rr.Code, rr.Body.String())
	}

	loginBody, err := json.Marshal(registerPayload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Token == "" {
		t.Fatalf("expected session token in login response")
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessionReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: resp.Token})
	sessionRec := httptest.NewRecorder()
	router.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusOK {
		t.Fatalf("expected session success via cookie, got %d: %s", sessionRec.Code, sessionRec.Body.String())
	}

	sessionResp := decodeResponse(t, sessionRec)
	if sessionResp.User == nil {
		t.Fatalf("expected user in session response")
	}
	if role, ok := sessionResp.User["role"].(string); !ok || role != store.RoleUser {
		t.Fatalf("expected persisted role %q, got %#v", store.RoleUser, sessionResp.User["role"])
	}
	if groups, ok := sessionResp.User["groups"].([]interface{}); !ok || len(groups) == 0 {
		t.Fatalf("expected session groups to be returned, got %#v", sessionResp.User["groups"])
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/auth/session", nil)
	deleteReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: resp.Token})
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete success via cookie, got %d: %s", deleteRec.Code, deleteRec.Body.String())
	}

	sessionReq = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessionReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: resp.Token})
	sessionRec = httptest.NewRecorder()
	router.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected session failure after deletion, got %d", sessionRec.Code)
	}
}

func TestMFATOTPFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	mailer := &testEmailSender{}
	RegisterRoutes(router, WithEmailSender(mailer))

	registerPayload := map[string]string{
		"name":     "Login User",
		"email":    "login@example.com",
		"password": "supersecure",
	}

	sendPayload := map[string]string{"email": registerPayload["email"]}
	sendBody, err := json.Marshal(sendPayload)
	if err != nil {
		t.Fatalf("failed to marshal send payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(sendBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification send success, got %d: %s", rr.Code, rr.Body.String())
	}

	msg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email during registration")
	}
	code := extractVerificationCodeFromMessage(t, msg)

	verifyPayload := map[string]string{
		"email": registerPayload["email"],
		"code":  code,
	}
	verifyBody, err := json.Marshal(verifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verify payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success, got %d: %s", rr.Code, rr.Body.String())
	}

	registerWithCode := map[string]string{
		"name":     registerPayload["name"],
		"email":    registerPayload["email"],
		"password": registerPayload["password"],
		"code":     code,
	}
	registerBody, err := json.Marshal(registerWithCode)
	if err != nil {
		t.Fatalf("failed to marshal registration payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected registration to succeed, got %d: %s", rr.Code, rr.Body.String())
	}

	loginPayload := map[string]string{
		"identifier": "Login User",
		"password":   registerPayload["password"],
	}
	loginBody, err := json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success for new user, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp.Token == "" {
		t.Fatalf("expected session token in login response")
	}
	if resp.MFAToken == "" {
		t.Fatalf("expected mfa token in login response")
	}
	if resp.User == nil {
		t.Fatalf("expected user object in login response")
	}

	provisionPayload := map[string]string{
		"token": resp.MFAToken,
	}
	provisionBody, err := json.Marshal(provisionPayload)
	if err != nil {
		t.Fatalf("failed to marshal provision payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/mfa/totp/provision", bytes.NewReader(provisionBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected provisioning success, got %d: %s", rr.Code, rr.Body.String())
	}
	resp = decodeResponse(t, rr)
	if resp.Secret == "" {
		t.Fatalf("expected totp secret in provisioning response")
	}
	if resp.Otpauth == "" {
		t.Fatalf("expected otpauth uri in provisioning response")
	}
	secret := resp.Secret

	preVerifyStatusReq := httptest.NewRequest(http.MethodGet, "/api/auth/mfa/status?"+url.Values{"identifier": {registerPayload["email"]}}.Encode(), nil)
	preVerifyStatusRec := httptest.NewRecorder()
	router.ServeHTTP(preVerifyStatusRec, preVerifyStatusReq)
	if preVerifyStatusRec.Code != http.StatusOK {
		t.Fatalf("expected identifier status success after provisioning, got %d: %s", preVerifyStatusRec.Code, preVerifyStatusRec.Body.String())
	}
	preVerifyStatusResp := decodeResponse(t, preVerifyStatusRec)
	if preVerifyStatusResp.MFA == nil {
		t.Fatalf("expected mfa state in identifier status response after provisioning")
	}
	if pending, ok := preVerifyStatusResp.MFA["totpPending"].(bool); !ok || !pending {
		t.Fatalf("expected identifier status to report totpPending true, got %#v", preVerifyStatusResp.MFA["totpPending"])
	}
	if issuedAt, ok := preVerifyStatusResp.MFA["totpSecretIssuedAt"].(string); !ok || strings.TrimSpace(issuedAt) == "" {
		t.Fatalf("expected identifier status to include totpSecretIssuedAt, got %#v", preVerifyStatusResp.MFA["totpSecretIssuedAt"])
	}

	generateCode := func(offset time.Duration) string {
		code, err := totp.GenerateCodeCustom(secret, time.Now().UTC().Add(offset), totp.ValidateOpts{
			Period:    30,
			Skew:      1,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			t.Fatalf("failed to generate verification code: %v", err)
		}
		return code
	}

	waitForStableTOTPWindow(t)
	mfaCode := generateCode(-30 * time.Second)

	totpVerifyPayload := map[string]string{
		"token": resp.MFAToken,
		"code":  mfaCode,
	}
	totpVerifyBody, err := json.Marshal(totpVerifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verify payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/mfa/totp/verify", bytes.NewReader(totpVerifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success, got %d: %s", rr.Code, rr.Body.String())
	}
	resp = decodeResponse(t, rr)
	if resp.Token == "" {
		t.Fatalf("expected session token after verification")
	}
	if resp.User == nil || resp.User["mfaEnabled"] != true {
		t.Fatalf("expected mfaEnabled true after verification")
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessionReq.Header.Set("Authorization", "Bearer "+resp.Token)
	sessionRec := httptest.NewRecorder()
	router.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusOK {
		t.Fatalf("expected session lookup success, got %d", sessionRec.Code)
	}
	sessionResp := decodeResponse(t, sessionRec)
	if sessionResp.User == nil {
		t.Fatalf("expected user in session response")
	}
	if sessionResp.User["mfaEnabled"] != true {
		t.Fatalf("expected session user to have mfaEnabled true")
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/auth/mfa/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+resp.Token)
	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status success, got %d", statusRec.Code)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/auth/session", nil)
	deleteReq.Header.Set("Authorization", "Bearer "+resp.Token)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected session deletion success, got %d", deleteRec.Code)
	}

	sessionReq = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessionReq.Header.Set("Authorization", "Bearer "+resp.Token)
	sessionRec = httptest.NewRecorder()
	router.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected session lookup failure after deletion, got %d", sessionRec.Code)
	}

	statusReq = httptest.NewRequest(http.MethodGet, "/api/auth/mfa/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+resp.Token)
	statusRec = httptest.NewRecorder()
	router.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status failure after session deletion, got %d", statusRec.Code)
	}

	loginWithTotp := func(body map[string]string) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal login payload: %v", err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	waitForStableTOTPWindow(t)
	totpCode := generateCode(-30 * time.Second)
	if ok, _ := totp.ValidateCustom(totpCode, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	}); !ok {
		t.Fatalf("locally generated totp code is invalid")
	}

	rr = loginWithTotp(map[string]string{
		"identifier": "Login User",
		"password":   registerPayload["password"],
		"totpCode":   totpCode,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected mfa login success, got %d: %s", rr.Code, rr.Body.String())
	}

	identifierStatusReq := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/mfa/status?"+url.Values{"identifier": {registerPayload["email"]}}.Encode(),
		nil,
	)
	identifierStatusRec := httptest.NewRecorder()
	router.ServeHTTP(identifierStatusRec, identifierStatusReq)
	if identifierStatusRec.Code != http.StatusOK {
		t.Fatalf("expected identifier status success, got %d: %s", identifierStatusRec.Code, identifierStatusRec.Body.String())
	}
	identifierStatusResp := decodeResponse(t, identifierStatusRec)
	if identifierStatusResp.MFA == nil {
		t.Fatalf("expected mfa payload in identifier status response")
	}
	if enabled, ok := identifierStatusResp.MFA["totpEnabled"].(bool); !ok || !enabled {
		t.Fatalf("expected identifier status to report totpEnabled true, got %#v", identifierStatusResp.MFA)
	}

	waitForStableTOTPWindow(t)
	totpCode = generateCode(0)
	if ok, _ := totp.ValidateCustom(totpCode, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	}); !ok {
		t.Fatalf("locally generated totp code is invalid (email login)")
	}

	rr = loginWithTotp(map[string]string{
		"identifier": registerPayload["email"],
		"totpCode":   totpCode,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected email+totp login success, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDisableMFA(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router, WithEmailVerification(false))

	registerPayload := map[string]string{
		"name":     "Disable User",
		"email":    "disable@example.com",
		"password": "disablePass1",
	}

	registerBody, err := json.Marshal(registerPayload)
	if err != nil {
		t.Fatalf("failed to marshal registration payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected registration success, got %d: %s", rr.Code, rr.Body.String())
	}

	loginPayload := map[string]string{
		"identifier": registerPayload["email"],
		"password":   registerPayload["password"],
	}
	loginBody, err := json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success for new user, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Token == "" {
		t.Fatalf("expected session token in login response")
	}
	if resp.MFAToken == "" {
		t.Fatalf("expected mfa token in login response")
	}

	provisionPayload := map[string]string{"token": resp.MFAToken}
	provisionBody, err := json.Marshal(provisionPayload)
	if err != nil {
		t.Fatalf("failed to marshal provision payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/mfa/totp/provision", bytes.NewReader(provisionBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected provisioning success, got %d: %s", rr.Code, rr.Body.String())
	}

	provisionResp := decodeResponse(t, rr)
	if provisionResp.Secret == "" {
		t.Fatalf("expected secret in provisioning response")
	}

	waitForStableTOTPWindow(t)
	code, err := totp.GenerateCodeCustom(provisionResp.Secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("failed to generate totp code: %v", err)
	}

	verifyPayload := map[string]string{
		"token": resp.MFAToken,
		"code":  code,
	}
	verifyBody, err := json.Marshal(verifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verify payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/mfa/totp/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success, got %d: %s", rr.Code, rr.Body.String())
	}

	verifyResp := decodeResponse(t, rr)
	if verifyResp.Token == "" {
		t.Fatalf("expected session token after verification")
	}

	disableReq := httptest.NewRequest(http.MethodPost, "/api/auth/mfa/disable", nil)
	disableReq.Header.Set("Authorization", "Bearer "+verifyResp.Token)
	disableRec := httptest.NewRecorder()
	router.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("expected disable success, got %d: %s", disableRec.Code, disableRec.Body.String())
	}

	disableResp := decodeResponse(t, disableRec)
	if disableResp.User == nil {
		t.Fatalf("expected user object in disable response")
	}
	if enabled, ok := disableResp.User["mfaEnabled"].(bool); ok && enabled {
		t.Fatalf("expected mfaEnabled false after disable, got %#v", enabled)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/auth/mfa/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+verifyResp.Token)
	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status success after disable, got %d: %s", statusRec.Code, statusRec.Body.String())
	}
	statusResp := decodeResponse(t, statusRec)
	if statusResp.MFA == nil {
		t.Fatalf("expected mfa state in status response")
	}
	if enabled, ok := statusResp.MFA["totpEnabled"].(bool); ok && enabled {
		t.Fatalf("expected totpEnabled false after disable, got %#v", enabled)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success after disable, got %d: %s", rr.Code, rr.Body.String())
	}
	resp = decodeResponse(t, rr)
	if resp.Token == "" {
		t.Fatalf("expected session token after disable login")
	}
	if resp.MFAToken == "" {
		t.Fatalf("expected mfa token after disable login")
	}
}

func TestHealthzEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected healthz endpoint to return 200, got %d", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode healthz response: %v", err)
	}
	if status := resp["status"]; status != "ok" {
		t.Fatalf("expected health status 'ok', got %q", status)
	}
}

func TestPingEndpointDerivesVersionFromImageEnv(t *testing.T) {
	t.Setenv("IMAGE", "ghcr.io/example/accounts:abcdef1234567890abcdef1234567890abcdef12")
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected ping endpoint to return 200, got %d", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode ping response: %v", err)
	}
	if got := resp["image"]; got != "ghcr.io/example/accounts:abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected image ref from env, got %q", got)
	}
	if got := resp["tag"]; got != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected tag derived from image ref, got %q", got)
	}
	if got := resp["commit"]; got != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected commit derived from image ref, got %q", got)
	}
	if got := resp["version"]; got != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected version derived from image ref, got %q", got)
	}
}

func TestPingEndpointDerivesCommitFromShaPrefixedImageTag(t *testing.T) {
	t.Setenv("IMAGE", "ghcr.io/example/accounts:sha-abcdef1234567890abcdef1234567890abcdef12")
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected ping endpoint to return 200, got %d", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode ping response: %v", err)
	}
	if got := resp["image"]; got != "ghcr.io/example/accounts:sha-abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected image ref from env, got %q", got)
	}
	if got := resp["tag"]; got != "sha-abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected tag derived from image ref, got %q", got)
	}
	if got := resp["commit"]; got != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected commit derived from sha-prefixed image ref, got %q", got)
	}
	if got := resp["version"]; got != "sha-abcdef1234567890abcdef1234567890abcdef12" {
		t.Fatalf("expected version derived from image ref, got %q", got)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	mailer := &testEmailSender{}
	RegisterRoutes(router, WithEmailSender(mailer))

	registerPayload := map[string]string{
		"name":     "Reset User",
		"email":    "reset@example.com",
		"password": "originalPass1",
	}

	sendPayload := map[string]string{"email": registerPayload["email"]}
	sendBody, err := json.Marshal(sendPayload)
	if err != nil {
		t.Fatalf("failed to marshal send payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register/send", bytes.NewReader(sendBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification send success, got %d: %s", rr.Code, rr.Body.String())
	}

	msg, ok := mailer.last()
	if !ok {
		t.Fatalf("expected verification email during registration")
	}
	verificationCode := extractVerificationCodeFromMessage(t, msg)

	verifyPayload := map[string]string{
		"email": registerPayload["email"],
		"code":  verificationCode,
	}
	verifyBody, err := json.Marshal(verifyPayload)
	if err != nil {
		t.Fatalf("failed to marshal verification payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/verify", bytes.NewReader(verifyBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected verification success, got %d: %s", rr.Code, rr.Body.String())
	}

	registerWithCode := map[string]string{
		"name":     registerPayload["name"],
		"email":    registerPayload["email"],
		"password": registerPayload["password"],
		"code":     verificationCode,
	}
	registerBody, err := json.Marshal(registerWithCode)
	if err != nil {
		t.Fatalf("failed to marshal registration payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected registration success, got %d: %s", rr.Code, rr.Body.String())
	}

	resetPayload := map[string]string{"email": registerPayload["email"]}
	resetBody, err := json.Marshal(resetPayload)
	if err != nil {
		t.Fatalf("failed to marshal reset payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/reset", bytes.NewReader(resetBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected password reset request to return 202, got %d: %s", rr.Code, rr.Body.String())
	}

	msg, ok = mailer.last()
	if !ok {
		t.Fatalf("expected password reset email to be sent")
	}
	if !strings.Contains(strings.ToLower(msg.Subject), "reset") {
		t.Fatalf("expected reset subject, got %q", msg.Subject)
	}
	resetToken := extractTokenFromMessage(t, msg)

	confirmPayload := map[string]string{
		"token":    resetToken,
		"password": "newSecurePass2",
	}
	confirmBody, err := json.Marshal(confirmPayload)
	if err != nil {
		t.Fatalf("failed to marshal confirm payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/password/reset/confirm", bytes.NewReader(confirmBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected password reset confirmation success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.User == nil {
		t.Fatalf("expected user in reset confirmation response")
	}
	if verified, ok := resp.User["emailVerified"].(bool); !ok || !verified {
		t.Fatalf("expected email to remain verified after reset")
	}

	loginPayload := map[string]string{
		"identifier": registerPayload["name"],
		"password":   confirmPayload["password"],
	}
	loginBody, err := json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success after password reset, got %d: %s", rr.Code, rr.Body.String())
	}
	resp = decodeResponse(t, rr)
	if resp.Token == "" {
		t.Fatalf("expected session token after password reset")
	}

	loginPayload["password"] = registerPayload["password"]
	loginBody, err = json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal old password payload: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected login with old password to fail, got %d", rr.Code)
	}
	resp = decodeResponse(t, rr)
	if resp.Error == "" {
		t.Fatalf("expected error when logging in with old password")
	}
}

func TestLoginSetsSessionCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	hashed, err := bcrypt.GenerateFromPassword([]byte("supersecure"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	user := &store.User{
		Name:          "cookie-user",
		Email:         "cookie@example.com",
		EmailVerified: true,
		PasswordHash:  string(hashed),
	}

	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	payload := map[string]string{
		"identifier": user.Email,
		"password":   "supersecure",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success, got %d: %s", rr.Code, rr.Body.String())
	}

	var sessionCookie *http.Cookie
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
			break
		}
	}

	if sessionCookie == nil {
		t.Fatalf("expected %s cookie to be set", sessionCookieName)
	}
	if sessionCookie.Value == "" {
		t.Fatalf("expected session cookie to have a value")
	}
	if !sessionCookie.HttpOnly {
		t.Fatalf("expected session cookie to be httpOnly")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.AddCookie(sessionCookie)

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected session retrieval success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.User == nil {
		t.Fatalf("expected user object in session response")
	}
	if id, ok := resp.User["id"].(string); !ok || id != user.ID {
		t.Fatalf("expected session user id %q, got %#v", user.ID, resp.User["id"])
	}
}

func TestLoginWithMFASetsSessionCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	hashed, err := bcrypt.GenerateFromPassword([]byte("supersecure"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "XControl",
		AccountName: "mfa@example.com",
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("failed to generate totp secret: %v", err)
	}

	now := time.Now().UTC()

	user := &store.User{
		Name:              "mfa-user",
		Email:             "mfa@example.com",
		EmailVerified:     true,
		PasswordHash:      string(hashed),
		MFAEnabled:        true,
		MFATOTPSecret:     key.Secret(),
		MFASecretIssuedAt: now,
		MFAConfirmedAt:    now,
	}

	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	waitForStableTOTPWindow(t)

	code, err := totp.GenerateCodeCustom(key.Secret(), time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("failed to generate totp code: %v", err)
	}

	payload := map[string]string{
		"identifier": user.Email,
		"password":   "supersecure",
		"totpCode":   code,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success, got %d: %s", rr.Code, rr.Body.String())
	}

	var sessionCookie *http.Cookie
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
			break
		}
	}

	if sessionCookie == nil {
		t.Fatalf("expected %s cookie to be set", sessionCookieName)
	}
	if sessionCookie.Value == "" {
		t.Fatalf("expected session cookie to have a value")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.AddCookie(sessionCookie)

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected session retrieval success, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.User == nil {
		t.Fatalf("expected user object in session response")
	}
	if id, ok := resp.User["id"].(string); !ok || id != user.ID {
		t.Fatalf("expected session user id %q, got %#v", user.ID, resp.User["id"])
	}
}

func TestAdminUsersMetricsForbiddenForStandardUser(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()
	called := false
	provider := &stubMetricsProvider{
		metrics: service.UserMetrics{},
		called:  &called,
	}

	RegisterRoutes(router, WithStore(st), WithEmailVerification(false), WithUserMetricsProvider(provider))

	testPass := "scrubbed"
	hashed, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	user := &store.User{
		ID:            "user-1",
		Name:          "standard",
		Email:         "user@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleUser,
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}

	loginPayload := map[string]string{
		"identifier": user.Email,
		"password":   testPass,
	}
	body, err := json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected login success, got %d: %s", rr.Code, rr.Body.String())
	}
	loginResp := decodeResponse(t, rr)
	if loginResp.Token == "" {
		t.Fatalf("expected session token from login response")
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/api/auth/admin/users/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+loginResp.Token)
	metricsRec := httptest.NewRecorder()
	router.ServeHTTP(metricsRec, metricsReq)

	if metricsRec.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden status, got %d: %s", metricsRec.Code, metricsRec.Body.String())
	}
	resp := decodeResponse(t, metricsRec)
	if resp.Error != "forbidden" {
		t.Fatalf("expected forbidden error code, got %q", resp.Error)
	}
	if called {
		t.Fatalf("metrics provider should not be invoked for unauthorized user")
	}
}

func TestAdminUsersMetricsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()

	expected := service.UserMetrics{
		Overview: service.MetricsOverview{
			TotalUsers:      10,
			ActiveUsers:     7,
			SubscribedUsers: 5,
			NewUsersLast24h: 3,
		},
		Series: service.MetricsSeries{
			Daily: []service.MetricsPoint{{
				Period:     "2024-03-17",
				Total:      2,
				Active:     1,
				Subscribed: 1,
			}},
			Weekly: []service.MetricsPoint{{
				Period:     "2024-W11",
				Total:      6,
				Active:     4,
				Subscribed: 3,
			}},
		},
	}
	provider := &stubMetricsProvider{metrics: expected}

	RegisterRoutes(router, WithStore(st), WithEmailVerification(false), WithUserMetricsProvider(provider))

	testPass := "scrubbed"
	hashed, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	admin := &store.User{
		ID:            "admin-1",
		Name:          "administrator",
		Email:         "admin@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleAdmin,
	}
	if err := st.CreateUser(context.Background(), admin); err != nil {
		t.Fatalf("failed to seed admin user: %v", err)
	}

	loginPayload := map[string]string{
		"identifier": admin.Email,
		"password":   testPass,
	}
	body, err := json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal admin login payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected admin login success, got %d: %s", rr.Code, rr.Body.String())
	}
	loginResp := decodeResponse(t, rr)
	if loginResp.Token == "" {
		t.Fatalf("expected session token from admin login response")
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/api/auth/admin/users/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+loginResp.Token)
	metricsRec := httptest.NewRecorder()
	router.ServeHTTP(metricsRec, metricsReq)

	if metricsRec.Code != http.StatusOK {
		t.Fatalf("expected metrics success, got %d: %s", metricsRec.Code, metricsRec.Body.String())
	}

	var payload service.UserMetrics
	if err := json.Unmarshal(metricsRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode metrics payload: %v", err)
	}
	if payload.Overview != expected.Overview {
		t.Fatalf("unexpected overview: %+v", payload.Overview)
	}
	if len(payload.Series.Daily) != len(expected.Series.Daily) || len(payload.Series.Weekly) != len(expected.Series.Weekly) {
		t.Fatalf("unexpected series lengths: %+v", payload.Series)
	}
	if payload.Series.Daily[0] != expected.Series.Daily[0] {
		t.Fatalf("unexpected daily series: %+v", payload.Series.Daily)
	}
	if payload.Series.Weekly[0] != expected.Series.Weekly[0] {
		t.Fatalf("unexpected weekly series: %+v", payload.Series.Weekly)
	}
}
