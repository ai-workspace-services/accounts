package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
)

type xworkmateAccessContext struct {
	Tenant              *store.Tenant
	Domain              string
	MembershipRole      string
	ProfileScope        string
	CanEditIntegrations bool
	CanManageTenant     bool
}

type xworkmateProfilePayload struct {
	BridgeServerURL    string                          `json:"BRIDGE_SERVER_URL"`
	BridgeServerOrigin string                          `json:"bridgeServerOrigin"`
	VaultURL           string                          `json:"vaultUrl"`
	VaultNamespace     string                          `json:"vaultNamespace"`
	VaultSecretPath    string                          `json:"vaultSecretPath"`
	VaultSecretKey     string                          `json:"vaultSecretKey"`
	SecretLocators     []xworkmateSecretLocatorPayload `json:"secretLocators"`
	ApisixURL          string                          `json:"apisixUrl"`
}

type xworkmateSecretLocatorPayload struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	SecretPath string `json:"secretPath"`
	SecretKey  string `json:"secretKey"`
	Target     string `json:"target"`
	Required   bool   `json:"required"`
}

var xworkmateForbiddenTokenFields = map[string]struct{}{
	"bridge_auth_token": {},
	"bridgeauthtoken":   {},
	"gatewaytoken":      {},
	"vaulttoken":        {},
	"apisixtoken":       {},
}

func (h *handler) ensureSharedXWorkmateTenant(ctx context.Context) error {
	tenant := &store.Tenant{
		ID:      store.SharedXWorkmateTenantID,
		Name:    store.SharedXWorkmateTenantName,
		Edition: store.SharedPublicTenantEdition,
	}
	if err := h.store.EnsureTenant(ctx, tenant); err != nil {
		return err
	}

	return h.store.EnsureTenantDomain(ctx, &store.TenantDomain{
		TenantID:  tenant.ID,
		Domain:    store.SharedXWorkmateDomain,
		Kind:      store.TenantDomainKindGenerated,
		IsPrimary: true,
		Status:    store.TenantDomainStatusVerified,
	})
}

func (h *handler) resolveTenantHost(c *gin.Context) string {
	for _, headerName := range []string{"X-Forwarded-Host", "X-Original-Host", "X-Host"} {
		if candidate := store.NormalizeHostname(c.GetHeader(headerName)); candidate != "" {
			return candidate
		}
	}
	if candidate := store.NormalizeHostname(c.Request.Host); candidate != "" {
		return candidate
	}
	return store.SharedXWorkmateDomain
}

func (h *handler) resolveFrontendURL(c *gin.Context) string {
	candidates := []string{
		strings.TrimSpace(c.Query("frontend_url")),
		strings.TrimSpace(c.GetHeader("X-Frontend-Url")),
		strings.TrimSpace(c.GetHeader("Origin")),
	}
	if referer := strings.TrimSpace(c.GetHeader("Referer")); referer != "" {
		if parsed, err := url.Parse(referer); err == nil && parsed.Host != "" {
			candidates = append(candidates, fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host))
		}
	}
	candidates = append(candidates, strings.TrimSpace(h.oauthFrontendURL))

	for _, candidate := range candidates {
		if validated := h.validateFrontendURL(candidate); validated != "" {
			return validated
		}
	}
	return ""
}

func (h *handler) validateFrontendURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}

	host := store.NormalizeHostname(parsed.Host)
	if host == "" {
		return ""
	}
	if store.IsSharedTenantHost(host) {
		return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	}
	if _, _, err := h.store.ResolveTenantByHost(context.Background(), host); err == nil {
		return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	}
	return ""
}

func buildOAuthState(frontendURL string) string {
	nonce := generateRandomState()
	trimmed := strings.TrimSpace(frontendURL)
	if trimmed == "" {
		return nonce
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(trimmed))
	return nonce + "." + encoded
}

func parseOAuthStateFrontendURL(state string) string {
	parts := strings.SplitN(strings.TrimSpace(state), ".", 2)
	if len(parts) != 2 {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(decoded))
}

func generateRandomState() string {
	return (&handler{}).generateState()
}

func (h *handler) currentAuthenticatedUser(c *gin.Context) (*store.User, bool) {
	userID := strings.TrimSpace(auth.GetUserID(c))
	if userID == "" || userID == "system" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return nil, false
	}

	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, http.StatusUnauthorized, "session_user_lookup_failed", "failed to load session user")
		return nil, false
	}
	if !user.Active {
		respondError(c, http.StatusForbidden, "account_suspended", "your account has been suspended")
		return nil, false
	}
	return user, true
}

func (h *handler) ensureSharedTenantMembership(ctx context.Context, user *store.User) (string, error) {
	role := store.TenantMembershipRoleUser
	if h.isRootAccount(user) ||
		strings.EqualFold(strings.TrimSpace(user.Role), store.RoleAdmin) ||
		strings.EqualFold(strings.TrimSpace(user.Role), store.RoleOperator) {
		role = store.TenantMembershipRoleAdmin
	}
	return role, h.store.UpsertTenantMembership(ctx, &store.TenantMembership{
		TenantID: store.SharedXWorkmateTenantID,
		UserID:   user.ID,
		Role:     role,
	})
}

func (h *handler) resolveXWorkmateAccess(ctx context.Context, host string, user *store.User) (*xworkmateAccessContext, error) {
	normalizedHost := store.NormalizeHostname(host)
	if store.IsSharedTenantHost(normalizedHost) {
		if err := h.ensureSharedXWorkmateTenant(ctx); err != nil {
			return nil, err
		}
	}

	tenant, domain, err := h.store.ResolveTenantByHost(ctx, normalizedHost)
	if err != nil {
		return nil, err
	}

	access := &xworkmateAccessContext{
		Tenant: tenant,
		Domain: store.SharedXWorkmateDomain,
	}
	if domain != nil && strings.TrimSpace(domain.Domain) != "" {
		access.Domain = domain.Domain
	}

	if tenant.Edition == store.SharedPublicTenantEdition {
		role, err := h.ensureSharedTenantMembership(ctx, user)
		if err != nil {
			return nil, err
		}
		access.MembershipRole = role
		access.ProfileScope = store.XWorkmateProfileScopeTenantShared
		access.CanEditIntegrations = role == store.TenantMembershipRoleAdmin
		access.CanManageTenant = access.CanEditIntegrations
		return access, nil
	}

	membership, err := h.store.GetTenantMembership(ctx, tenant.ID, user.ID)
	if err != nil {
		return nil, err
	}

	access.MembershipRole = membership.Role
	access.ProfileScope = store.XWorkmateProfileScopeUserPrivate
	access.CanEditIntegrations = true
	access.CanManageTenant = membership.Role == store.TenantMembershipRoleAdmin
	return access, nil
}

func buildSessionTenantEntries(memberships []store.TenantMembership) []gin.H {
	if len(memberships) == 0 {
		return []gin.H{}
	}

	result := make([]gin.H, 0, len(memberships))
	for _, membership := range memberships {
		entry := gin.H{
			"id":   membership.TenantID,
			"role": membership.Role,
		}
		if strings.TrimSpace(membership.TenantName) != "" {
			entry["name"] = membership.TenantName
		}
		result = append(result, entry)
	}
	return result
}

func buildXWorkmateTokenConfigured(profile *store.XWorkmateProfile) gin.H {
	result := gin.H{
		"bridge": false,
		"vault":  false,
		"apisix": false,
	}
	if profile == nil {
		return result
	}

	if hasBridgeAuthTokenXWorkmateSecretLocator(profile) {
		result["bridge"] = true
	}

	return result
}

func buildXWorkmateTokenConfiguredWithVaultStatus(profile *store.XWorkmateProfile, vaultStatus map[string]bool) gin.H {
	result := buildXWorkmateTokenConfigured(profile)
	if len(vaultStatus) == 0 {
		return result
	}

	if configured, ok := vaultStatus[store.XWorkmateSecretLocatorTargetBridgeAuthToken]; ok {
		result["bridge"] = configured
	}
	if configured, ok := vaultStatus[store.XWorkmateSecretLocatorTargetVaultRootToken]; ok {
		result["vault"] = configured
	}
	if configured, ok := vaultStatus[store.XWorkmateSecretLocatorTargetAIGatewayAccessToken]; ok {
		result["apisix"] = configured
	}

	return result
}

func hasBridgeAuthTokenXWorkmateSecretLocator(profile *store.XWorkmateProfile) bool {
	if profile == nil {
		return false
	}

	if strings.TrimSpace(profile.VaultSecretPath) != "" && strings.TrimSpace(profile.VaultSecretKey) != "" {
		return true
	}
	for _, locator := range profile.SecretLocators {
		if locator.Target != store.XWorkmateSecretLocatorTargetBridgeAuthToken {
			continue
		}
		if strings.TrimSpace(locator.SecretPath) != "" && strings.TrimSpace(locator.SecretKey) != "" {
			return true
		}
	}
	return false
}

func buildXWorkmateSecretLocators(profile *store.XWorkmateProfile) []gin.H {
	if profile == nil || len(profile.SecretLocators) == 0 {
		return []gin.H{}
	}

	result := make([]gin.H, 0, len(profile.SecretLocators))
	for _, locator := range profile.SecretLocators {
		entry := gin.H{
			"id":         locator.ID,
			"provider":   locator.Provider,
			"secretPath": locator.SecretPath,
			"secretKey":  locator.SecretKey,
			"target":     locator.Target,
			"required":   locator.Required,
		}
		result = append(result, entry)
	}
	return result
}

func buildStoreXWorkmateSecretLocators(locators []xworkmateSecretLocatorPayload) []store.XWorkmateSecretLocator {
	if len(locators) == 0 {
		return []store.XWorkmateSecretLocator{}
	}

	result := make([]store.XWorkmateSecretLocator, 0, len(locators))
	for _, locator := range locators {
		result = append(result, store.XWorkmateSecretLocator{
			ID:         locator.ID,
			Provider:   locator.Provider,
			SecretPath: locator.SecretPath,
			SecretKey:  locator.SecretKey,
			Target:     locator.Target,
			Required:   locator.Required,
		})
	}
	return result
}

func (h *handler) buildSessionUser(ctx context.Context, host string, user *store.User) (gin.H, error) {
	access, err := h.resolveXWorkmateAccess(ctx, host, user)
	if err != nil {
		return nil, err
	}

	memberships, err := h.store.ListTenantMembershipsByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	payload := sanitizeUser(user, nil)
	payload["tenantId"] = access.Tenant.ID
	payload["tenants"] = buildSessionTenantEntries(memberships)
	return payload, nil
}

func buildXWorkmateProfileResponse(access *xworkmateAccessContext, profile *store.XWorkmateProfile, tokenConfigured gin.H) gin.H {
	resolvedProfile := gin.H{
		"BRIDGE_SERVER_URL":  "",
		"bridgeServerOrigin": "",
		"vaultUrl":           "",
		"vaultNamespace":     "",
		"vaultSecretPath":    "",
		"vaultSecretKey":     "",
		"secretLocators":     []gin.H{},
		"apisixUrl":          "",
	}
	if profile != nil {
		resolvedProfile["BRIDGE_SERVER_URL"] = profile.BridgeServerURL
		resolvedProfile["bridgeServerOrigin"] = profile.BridgeServerOrigin
		resolvedProfile["vaultUrl"] = profile.VaultURL
		resolvedProfile["vaultNamespace"] = profile.VaultNamespace
		resolvedProfile["vaultSecretPath"] = profile.VaultSecretPath
		resolvedProfile["vaultSecretKey"] = profile.VaultSecretKey
		resolvedProfile["secretLocators"] = buildXWorkmateSecretLocators(profile)
		resolvedProfile["apisixUrl"] = profile.ApisixURL
	}

	return gin.H{
		"edition": access.Tenant.Edition,
		"tenant": gin.H{
			"id":     access.Tenant.ID,
			"name":   access.Tenant.Name,
			"domain": access.Domain,
		},
		"membershipRole":      access.MembershipRole,
		"profileScope":        access.ProfileScope,
		"canEditIntegrations": access.CanEditIntegrations,
		"canManageTenant":     access.CanManageTenant,
		"profile":             resolvedProfile,
		"tokenConfigured":     tokenConfigured,
	}
}

func resolvedXWorkmateProfileUserID(access *xworkmateAccessContext, user *store.User) string {
	if access == nil {
		return ""
	}
	if access.ProfileScope == store.XWorkmateProfileScopeTenantShared {
		return ""
	}
	if user == nil {
		return ""
	}
	return strings.TrimSpace(user.ID)
}

func (h *handler) loadXWorkmateProfile(ctx context.Context, access *xworkmateAccessContext, user *store.User) (*store.XWorkmateProfile, error) {
	profileUserID := resolvedXWorkmateProfileUserID(access, user)
	profile, err := h.store.GetXWorkmateProfile(ctx, access.Tenant.ID, profileUserID, access.ProfileScope)
	if err == nil {
		return profile, nil
	}
	if !errors.Is(err, store.ErrXWorkmateProfileNotFound) {
		return nil, err
	}
	if access.ProfileScope != store.XWorkmateProfileScopeTenantShared {
		return nil, nil
	}

	legacyProfile, legacyErr := h.store.GetXWorkmateProfile(ctx, access.Tenant.ID, strings.TrimSpace(user.ID), access.ProfileScope)
	if legacyErr != nil {
		if errors.Is(legacyErr, store.ErrXWorkmateProfileNotFound) {
			return nil, nil
		}
		return nil, legacyErr
	}
	return legacyProfile, nil
}

func (h *handler) ensureXWorkmateVaultService(c *gin.Context) bool {
	if h.xworkmateVaultService != nil {
		return true
	}
	respondError(c, http.StatusServiceUnavailable, "xworkmate_vault_unavailable", "xworkmate vault integration is not configured")
	return false
}

func findStoredXWorkmateSecretLocator(profile *store.XWorkmateProfile, target string) (store.XWorkmateSecretLocator, bool) {
	if profile == nil {
		return store.XWorkmateSecretLocator{}, false
	}
	normalizedTarget := strings.ToLower(strings.TrimSpace(target))
	for _, locator := range profile.SecretLocators {
		if locator.Target != normalizedTarget {
			continue
		}
		if strings.TrimSpace(locator.SecretPath) == "" || strings.TrimSpace(locator.SecretKey) == "" {
			continue
		}
		store.NormalizeXWorkmateSecretLocator(&locator)
		return locator, true
	}
	return store.XWorkmateSecretLocator{}, false
}

func upsertXWorkmateSecretLocator(profile *store.XWorkmateProfile, locator store.XWorkmateSecretLocator) {
	if profile == nil {
		return
	}
	store.NormalizeXWorkmateSecretLocator(&locator)
	for i := range profile.SecretLocators {
		if profile.SecretLocators[i].Target != locator.Target {
			continue
		}
		profile.SecretLocators[i].ID = locator.ID
		profile.SecretLocators[i].Provider = locator.Provider
		profile.SecretLocators[i].SecretPath = locator.SecretPath
		profile.SecretLocators[i].SecretKey = locator.SecretKey
		profile.SecretLocators[i].Required = locator.Required
		store.NormalizeXWorkmateProfile(profile)
		return
	}
	profile.SecretLocators = append(profile.SecretLocators, locator)
	store.NormalizeXWorkmateProfile(profile)
}

func buildXWorkmateSecretStatusPayload(locator store.XWorkmateSecretLocator, configured bool) gin.H {
	managedTarget, _ := findXWorkmateManagedTarget(locator.Target)
	return gin.H{
		"target":     locator.Target,
		"configured": configured,
		"state": func() string {
			if configured {
				return "configured"
			}
			return "missing"
		}(),
		"required": managedTarget.Required || locator.Required,
		"locator": gin.H{
			"id":         locator.ID,
			"provider":   locator.Provider,
			"secretPath": locator.SecretPath,
			"secretKey":  locator.SecretKey,
			"target":     locator.Target,
			"required":   managedTarget.Required || locator.Required,
		},
	}
}

func (h *handler) describeXWorkmateSecrets(ctx context.Context, access *xworkmateAccessContext, user *store.User, profile *store.XWorkmateProfile) ([]gin.H, map[string]bool, error) {
	profileUserID := resolvedXWorkmateProfileUserID(access, user)
	secrets := make([]gin.H, 0, len(xworkmateManagedSecretTargets))
	statusByTarget := make(map[string]bool, len(xworkmateManagedSecretTargets))

	for _, managedTarget := range xworkmateManagedSecretTargets {
		locator, ok := findStoredXWorkmateSecretLocator(profile, managedTarget.Target)
		if !ok {
			var err error
			locator, err = buildManagedXWorkmateSecretLocator(access, profileUserID, managedTarget.Target)
			if err != nil {
				return nil, nil, err
			}
		}

		configured := false
		if h.xworkmateVaultService != nil {
			var err error
			configured, err = h.xworkmateVaultService.HasSecret(ctx, locator)
			if err != nil {
				return nil, nil, err
			}
		} else {
			configured = statusByTargetFromMetadata(profile, managedTarget.Target)
		}

		statusByTarget[managedTarget.Target] = configured
		secrets = append(secrets, buildXWorkmateSecretStatusPayload(locator, configured))
	}

	return secrets, statusByTarget, nil
}

func statusByTargetFromMetadata(profile *store.XWorkmateProfile, target string) bool {
	if profile == nil {
		return false
	}
	if target == store.XWorkmateSecretLocatorTargetBridgeAuthToken {
		return hasBridgeAuthTokenXWorkmateSecretLocator(profile)
	}
	for _, locator := range profile.SecretLocators {
		if locator.Target == strings.ToLower(strings.TrimSpace(target)) &&
			strings.TrimSpace(locator.SecretPath) != "" &&
			strings.TrimSpace(locator.SecretKey) != "" {
			return true
		}
	}
	return false
}

func containsForbiddenXWorkmateTokenField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if _, ok := xworkmateForbiddenTokenFields[strings.ToLower(strings.TrimSpace(key))]; ok {
				return true
			}
			if containsForbiddenXWorkmateTokenField(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsForbiddenXWorkmateTokenField(nested) {
				return true
			}
		}
	}
	return false
}

func (h *handler) getXWorkmateProfile(c *gin.Context) {
	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}

	access, err := h.resolveXWorkmateAccess(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantMembershipNotFound) {
			respondError(c, http.StatusForbidden, "tenant_membership_required", "tenant membership is required")
			return
		}
		if errors.Is(err, store.ErrTenantNotFound) {
			respondError(c, http.StatusNotFound, "tenant_not_found", "tenant was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "xworkmate_context_failed", "failed to resolve xworkmate context")
		return
	}

	profile, err := h.loadXWorkmateProfile(c.Request.Context(), access, user)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_read_failed", "failed to load xworkmate profile")
		return
	}

	tokenConfigured := buildXWorkmateTokenConfigured(profile)
	if h.xworkmateVaultService != nil {
		_, statusByTarget, err := h.describeXWorkmateSecrets(c.Request.Context(), access, user, profile)
		if err == nil {
			tokenConfigured = buildXWorkmateTokenConfiguredWithVaultStatus(profile, statusByTarget)
		}
	}

	c.JSON(http.StatusOK, buildXWorkmateProfileResponse(access, profile, tokenConfigured))
}

func (h *handler) getXWorkmateProfileSync(c *gin.Context) {
	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}

	access, err := h.resolveXWorkmateAccess(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantMembershipNotFound) {
			respondError(c, http.StatusForbidden, "tenant_membership_required", "tenant membership is required")
			return
		}
		if errors.Is(err, store.ErrTenantNotFound) {
			respondError(c, http.StatusNotFound, "tenant_not_found", "tenant was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "xworkmate_context_failed", "failed to resolve xworkmate context")
		return
	}

	profile, err := h.loadXWorkmateProfile(c.Request.Context(), access, user)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_read_failed", "failed to load xworkmate profile")
		return
	}

	bridgeServerURL := ""
	if profile != nil {
		bridgeServerURL = strings.TrimSpace(profile.BridgeServerURL)
	}
	if bridgeServerURL == "" {
		respondError(c, http.StatusConflict, "bridge_server_url_unavailable", "bridge server url is unavailable")
		return
	}
	if h.xworkmateVaultService == nil {
		respondError(c, http.StatusConflict, "bridge_auth_token_unavailable", "bridge auth token is unavailable")
		return
	}

	locator, ok := findStoredXWorkmateSecretLocator(profile, store.XWorkmateSecretLocatorTargetBridgeAuthToken)
	if !ok {
		respondError(c, http.StatusConflict, "bridge_auth_token_unavailable", "bridge auth token is unavailable")
		return
	}

	bridgeAuthToken, err := h.xworkmateVaultService.ReadSecret(c.Request.Context(), locator)
	if err != nil || strings.TrimSpace(bridgeAuthToken) == "" {
		respondError(c, http.StatusConflict, "bridge_auth_token_unavailable", "bridge auth token is unavailable")
		return
	}
	if isReviewXWorkmateAccount(user) {
		reviewToken := strings.TrimSpace(os.Getenv("BRIDGE_REVIEW_AUTH_TOKEN"))
		if reviewToken == "" {
			respondError(c, http.StatusConflict, "bridge_review_auth_token_unavailable", "bridge review auth token is unavailable")
			return
		}
		bridgeAuthToken = reviewToken
	}

	c.JSON(http.StatusOK, gin.H{
		"BRIDGE_SERVER_URL": bridgeServerURL,
		"BRIDGE_AUTH_TOKEN": strings.TrimSpace(bridgeAuthToken),
	})
}

func isReviewXWorkmateAccount(user *store.User) bool {
	if user == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(user.Email), "review@svc.plus")
}

func (h *handler) updateXWorkmateProfile(c *gin.Context) {
	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}

	access, err := h.resolveXWorkmateAccess(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantMembershipNotFound) {
			respondError(c, http.StatusForbidden, "tenant_membership_required", "tenant membership is required")
			return
		}
		if errors.Is(err, store.ErrTenantNotFound) {
			respondError(c, http.StatusNotFound, "tenant_not_found", "tenant was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "xworkmate_context_failed", "failed to resolve xworkmate context")
		return
	}

	if !access.CanEditIntegrations {
		respondError(c, http.StatusForbidden, "xworkmate_profile_forbidden", "you are not allowed to update integrations for this tenant")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	var raw map[string]any
	if err := c.ShouldBindJSON(&raw); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	if containsForbiddenXWorkmateTokenField(raw) {
		respondError(c, http.StatusBadRequest, "token_persistence_forbidden", "raw token fields cannot be persisted")
		return
	}

	profileValue, ok := raw["profile"]
	if !ok {
		profileValue = raw
	}

	encodedProfile, err := json.Marshal(profileValue)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid profile payload")
		return
	}

	var payload xworkmateProfilePayload
	if err := json.Unmarshal(encodedProfile, &payload); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid profile payload")
		return
	}

	profileUserID := resolvedXWorkmateProfileUserID(access, user)

	profile := &store.XWorkmateProfile{
		TenantID:           access.Tenant.ID,
		UserID:             profileUserID,
		Scope:              access.ProfileScope,
		BridgeServerURL:    payload.BridgeServerURL,
		BridgeServerOrigin: payload.BridgeServerOrigin,
		VaultURL:           payload.VaultURL,
		VaultNamespace:     payload.VaultNamespace,
		VaultSecretPath:    payload.VaultSecretPath,
		VaultSecretKey:     payload.VaultSecretKey,
		SecretLocators:     buildStoreXWorkmateSecretLocators(payload.SecretLocators),
		ApisixURL:          payload.ApisixURL,
	}
	if err := h.store.UpsertXWorkmateProfile(c.Request.Context(), profile); err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_write_failed", "failed to save xworkmate profile")
		return
	}

	tokenConfigured := buildXWorkmateTokenConfigured(profile)
	if h.xworkmateVaultService != nil {
		_, statusByTarget, err := h.describeXWorkmateSecrets(c.Request.Context(), access, user, profile)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "xworkmate_secret_read_failed", "failed to load xworkmate secret status")
			return
		}
		tokenConfigured = buildXWorkmateTokenConfiguredWithVaultStatus(profile, statusByTarget)
	}

	c.JSON(http.StatusOK, buildXWorkmateProfileResponse(access, profile, tokenConfigured))
}

func (h *handler) getXWorkmateSecrets(c *gin.Context) {
	if !h.ensureXWorkmateVaultService(c) {
		return
	}

	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}

	access, err := h.resolveXWorkmateAccess(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantMembershipNotFound) {
			respondError(c, http.StatusForbidden, "tenant_membership_required", "tenant membership is required")
			return
		}
		if errors.Is(err, store.ErrTenantNotFound) {
			respondError(c, http.StatusNotFound, "tenant_not_found", "tenant was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "xworkmate_context_failed", "failed to resolve xworkmate context")
		return
	}

	profile, err := h.loadXWorkmateProfile(c.Request.Context(), access, user)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_read_failed", "failed to load xworkmate profile")
		return
	}

	secrets, statusByTarget, err := h.describeXWorkmateSecrets(c.Request.Context(), access, user, profile)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_secret_read_failed", "failed to load xworkmate secret status")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"edition":             access.Tenant.Edition,
		"profileScope":        access.ProfileScope,
		"membershipRole":      access.MembershipRole,
		"canEditIntegrations": access.CanEditIntegrations,
		"canManageTenant":     access.CanManageTenant,
		"tenant":              gin.H{"id": access.Tenant.ID, "name": access.Tenant.Name, "domain": access.Domain},
		"secrets":             secrets,
		"tokenConfigured":     buildXWorkmateTokenConfiguredWithVaultStatus(profile, statusByTarget),
		"vaultBackendEnabled": true,
	})
}

func (h *handler) putXWorkmateSecret(c *gin.Context) {
	if !h.ensureXWorkmateVaultService(c) {
		return
	}

	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}

	access, err := h.resolveXWorkmateAccess(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantMembershipNotFound) {
			respondError(c, http.StatusForbidden, "tenant_membership_required", "tenant membership is required")
			return
		}
		if errors.Is(err, store.ErrTenantNotFound) {
			respondError(c, http.StatusNotFound, "tenant_not_found", "tenant was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "xworkmate_context_failed", "failed to resolve xworkmate context")
		return
	}
	if !access.CanEditIntegrations {
		respondError(c, http.StatusForbidden, "xworkmate_secret_forbidden", "you are not allowed to update integrations for this tenant")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	target := strings.ToLower(strings.TrimSpace(c.Param("target")))
	if _, ok := findXWorkmateManagedTarget(target); !ok {
		respondError(c, http.StatusBadRequest, "xworkmate_secret_unknown_target", "unknown xworkmate secret target")
		return
	}

	var payload struct {
		Value string `json:"value"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	if strings.TrimSpace(payload.Value) == "" {
		respondError(c, http.StatusBadRequest, "xworkmate_secret_value_required", "secret value is required")
		return
	}

	profile, err := h.loadXWorkmateProfile(c.Request.Context(), access, user)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_read_failed", "failed to load xworkmate profile")
		return
	}
	if profile == nil {
		profile = &store.XWorkmateProfile{
			TenantID: access.Tenant.ID,
			UserID:   resolvedXWorkmateProfileUserID(access, user),
			Scope:    access.ProfileScope,
		}
	}

	locator, err := buildManagedXWorkmateSecretLocator(access, resolvedXWorkmateProfileUserID(access, user), target)
	if err != nil {
		respondError(c, http.StatusBadRequest, "xworkmate_secret_unknown_target", "unknown xworkmate secret target")
		return
	}
	if err := h.xworkmateVaultService.WriteSecret(c.Request.Context(), locator, payload.Value); err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_secret_write_failed", "failed to persist xworkmate secret")
		return
	}

	upsertXWorkmateSecretLocator(profile, locator)
	if err := h.store.UpsertXWorkmateProfile(c.Request.Context(), profile); err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_write_failed", "failed to save xworkmate profile")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"secret":          buildXWorkmateSecretStatusPayload(locator, true),
		"profileScope":    access.ProfileScope,
		"tokenConfigured": buildXWorkmateTokenConfiguredWithVaultStatus(profile, map[string]bool{target: true}),
	})
}

func (h *handler) deleteXWorkmateSecret(c *gin.Context) {
	if !h.ensureXWorkmateVaultService(c) {
		return
	}

	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}

	access, err := h.resolveXWorkmateAccess(c.Request.Context(), h.resolveTenantHost(c), user)
	if err != nil {
		if errors.Is(err, store.ErrTenantMembershipNotFound) {
			respondError(c, http.StatusForbidden, "tenant_membership_required", "tenant membership is required")
			return
		}
		if errors.Is(err, store.ErrTenantNotFound) {
			respondError(c, http.StatusNotFound, "tenant_not_found", "tenant was not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "xworkmate_context_failed", "failed to resolve xworkmate context")
		return
	}
	if !access.CanEditIntegrations {
		respondError(c, http.StatusForbidden, "xworkmate_secret_forbidden", "you are not allowed to update integrations for this tenant")
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}

	target := strings.ToLower(strings.TrimSpace(c.Param("target")))
	if _, ok := findXWorkmateManagedTarget(target); !ok {
		respondError(c, http.StatusBadRequest, "xworkmate_secret_unknown_target", "unknown xworkmate secret target")
		return
	}

	profile, err := h.loadXWorkmateProfile(c.Request.Context(), access, user)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_profile_read_failed", "failed to load xworkmate profile")
		return
	}

	locator, ok := findStoredXWorkmateSecretLocator(profile, target)
	if !ok {
		locator, err = buildManagedXWorkmateSecretLocator(access, resolvedXWorkmateProfileUserID(access, user), target)
		if err != nil {
			respondError(c, http.StatusBadRequest, "xworkmate_secret_unknown_target", "unknown xworkmate secret target")
			return
		}
	}

	if err := h.xworkmateVaultService.DeleteSecret(c.Request.Context(), locator); err != nil {
		respondError(c, http.StatusInternalServerError, "xworkmate_secret_delete_failed", "failed to delete xworkmate secret")
		return
	}

	tokenConfigured := buildXWorkmateTokenConfigured(profile)
	if h.xworkmateVaultService != nil {
		_, statusByTarget, err := h.describeXWorkmateSecrets(c.Request.Context(), access, user, profile)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "xworkmate_secret_read_failed", "failed to load xworkmate secret status")
			return
		}
		tokenConfigured = buildXWorkmateTokenConfiguredWithVaultStatus(profile, statusByTarget)
	}

	c.JSON(http.StatusOK, gin.H{
		"secret":          buildXWorkmateSecretStatusPayload(locator, false),
		"profileScope":    access.ProfileScope,
		"tokenConfigured": tokenConfigured,
	})
}

func (h *handler) bootstrapTenant(c *gin.Context) {
	adminUser, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !h.isRootAccount(adminUser) {
		respondError(c, http.StatusForbidden, "root_only", "root only")
		return
	}

	var payload struct {
		Name        string `json:"name"`
		AdminUserID string `json:"adminUserId"`
		AdminEmail  string `json:"adminEmail"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	var member *store.User
	var err error
	switch {
	case strings.TrimSpace(payload.AdminUserID) != "":
		member, err = h.store.GetUserByID(c.Request.Context(), strings.TrimSpace(payload.AdminUserID))
	case strings.TrimSpace(payload.AdminEmail) != "":
		member, err = h.store.GetUserByEmail(c.Request.Context(), strings.TrimSpace(payload.AdminEmail))
	default:
		respondError(c, http.StatusBadRequest, "admin_user_required", "adminUserId or adminEmail is required")
		return
	}
	if err != nil {
		respondError(c, http.StatusNotFound, "admin_user_not_found", "admin user not found")
		return
	}

	domain, err := store.GenerateRandomTenantDomain()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "tenant_domain_generation_failed", "failed to generate tenant domain")
		return
	}

	tenant := &store.Tenant{
		Name:    strings.TrimSpace(payload.Name),
		Edition: store.TenantPrivateEdition,
	}
	if tenant.Name == "" {
		tenant.Name = member.Name
		if tenant.Name == "" {
			tenant.Name = member.Email
		}
	}
	if err := h.store.EnsureTenant(c.Request.Context(), tenant); err != nil {
		respondError(c, http.StatusInternalServerError, "tenant_create_failed", "failed to create tenant")
		return
	}
	if err := h.store.EnsureTenantDomain(c.Request.Context(), &store.TenantDomain{
		TenantID:  tenant.ID,
		Domain:    domain,
		Kind:      store.TenantDomainKindGenerated,
		IsPrimary: true,
		Status:    store.TenantDomainStatusVerified,
	}); err != nil {
		respondError(c, http.StatusInternalServerError, "tenant_domain_create_failed", "failed to create tenant domain")
		return
	}
	if err := h.store.UpsertTenantMembership(c.Request.Context(), &store.TenantMembership{
		TenantID: tenant.ID,
		UserID:   member.ID,
		Role:     store.TenantMembershipRoleAdmin,
	}); err != nil {
		respondError(c, http.StatusInternalServerError, "tenant_membership_create_failed", "failed to create tenant membership")
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"tenant": gin.H{
			"id":      tenant.ID,
			"name":    tenant.Name,
			"edition": tenant.Edition,
			"domain":  domain,
		},
		"member": gin.H{
			"id":    member.ID,
			"email": member.Email,
			"role":  store.TenantMembershipRoleAdmin,
		},
	})
}
