package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SharedPublicTenantEdition = "shared_public"
	TenantPrivateEdition      = "tenant_private"

	TenantMembershipRoleAdmin = "admin"
	TenantMembershipRoleUser  = "user"

	TenantDomainKindGenerated = "generated"
	TenantDomainKindCustom    = "custom"

	TenantDomainStatusPending  = "pending"
	TenantDomainStatusVerified = "verified"

	XWorkmateProfileScopeTenantShared = "tenant-shared"
	XWorkmateProfileScopeUserPrivate  = "user-private"

	XWorkmateSecretLocatorProviderVault = "vault"

	XWorkmateSecretLocatorTargetBridgeAuthToken      = "bridge.auth_token"
	XWorkmateSecretLocatorTargetOpenclawGatewayToken = XWorkmateSecretLocatorTargetBridgeAuthToken
	XWorkmateSecretLocatorTargetVaultRootToken       = "vault.root_token"
	XWorkmateSecretLocatorTargetAIGatewayAccessToken = "ai_gateway.access_token"
	XWorkmateSecretLocatorTargetOllamaCloudAPIKey    = "ollama_cloud.api_key"

	// This stable identifier preserves the existing shared tenant row during
	// environment-domain migration; it is an opaque ID, not a public domain.
	SharedXWorkmateTenantID   = "svc-plus-xworkmate"
	SharedXWorkmateTenantName = "Shared XWorkmate"
)

var (
	ErrTenantNotFound           = errors.New("tenant not found")
	ErrTenantMembershipNotFound = errors.New("tenant membership not found")
	ErrXWorkmateProfileNotFound = errors.New("xworkmate profile not found")
)

type Tenant struct {
	ID        string
	Name      string
	Edition   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type TenantDomain struct {
	ID        string
	TenantID  string
	Domain    string
	Kind      string
	IsPrimary bool
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type TenantMembership struct {
	TenantID      string
	UserID        string
	Role          string
	TenantName    string
	TenantEdition string
	Domain        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type XWorkmateProfile struct {
	ID                 string
	TenantID           string
	UserID             string
	Scope              string
	BridgeServerURL    string
	BridgeServerOrigin string
	VaultURL           string
	VaultNamespace     string
	VaultSecretPath    string
	VaultSecretKey     string
	SecretLocators     []XWorkmateSecretLocator
	ApisixURL          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type XWorkmateSecretLocator struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	SecretPath string `json:"secretPath"`
	SecretKey  string `json:"secretKey"`
	Target     string `json:"target"`
	Required   bool   `json:"required"`
}

func NormalizeTenantEdition(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SharedPublicTenantEdition:
		return SharedPublicTenantEdition
	default:
		return TenantPrivateEdition
	}
}

func NormalizeTenantMembershipRole(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TenantMembershipRoleAdmin:
		return TenantMembershipRoleAdmin
	default:
		return TenantMembershipRoleUser
	}
}

func NormalizeTenantDomainKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TenantDomainKindCustom:
		return TenantDomainKindCustom
	default:
		return TenantDomainKindGenerated
	}
}

func NormalizeTenantDomainStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TenantDomainStatusPending:
		return TenantDomainStatusPending
	default:
		return TenantDomainStatusVerified
	}
}

func NormalizeXWorkmateProfileScope(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case XWorkmateProfileScopeTenantShared:
		return XWorkmateProfileScopeTenantShared
	default:
		return XWorkmateProfileScopeUserPrivate
	}
}

func NormalizeXWorkmateSecretLocator(locator *XWorkmateSecretLocator) {
	if locator == nil {
		return
	}

	locator.ID = strings.TrimSpace(locator.ID)
	if locator.ID == "" {
		locator.ID = uuid.NewString()
	}
	locator.Provider = strings.ToLower(strings.TrimSpace(locator.Provider))
	if locator.Provider == "" {
		locator.Provider = XWorkmateSecretLocatorProviderVault
	}
	locator.SecretPath = strings.Trim(strings.TrimSpace(locator.SecretPath), "/")
	locator.SecretKey = strings.TrimSpace(locator.SecretKey)
	locator.Target = strings.ToLower(strings.TrimSpace(locator.Target))
}

func cloneXWorkmateSecretLocators(locators []XWorkmateSecretLocator) []XWorkmateSecretLocator {
	if len(locators) == 0 {
		return []XWorkmateSecretLocator{}
	}

	cloned := make([]XWorkmateSecretLocator, len(locators))
	copy(cloned, locators)
	return cloned
}

func legacyXWorkmateSecretLocatorID(profile *XWorkmateProfile) string {
	if profile == nil {
		return "legacy|xworkmate|bridge.auth_token"
	}

	return strings.Join([]string{
		"legacy",
		strings.TrimSpace(profile.TenantID),
		strings.TrimSpace(profile.UserID),
		NormalizeXWorkmateProfileScope(profile.Scope),
		XWorkmateSecretLocatorTargetBridgeAuthToken,
	}, "|")
}

func synthesizeXWorkmateSecretLocatorFromLegacy(profile *XWorkmateProfile) XWorkmateSecretLocator {
	return XWorkmateSecretLocator{
		ID:         legacyXWorkmateSecretLocatorID(profile),
		Provider:   XWorkmateSecretLocatorProviderVault,
		SecretPath: profile.VaultSecretPath,
		SecretKey:  profile.VaultSecretKey,
		Target:     XWorkmateSecretLocatorTargetBridgeAuthToken,
	}
}

func compatibilityXWorkmateSecretLocator(locators []XWorkmateSecretLocator) (string, string, bool) {
	for _, locator := range locators {
		if locator.Target == XWorkmateSecretLocatorTargetBridgeAuthToken &&
			locator.SecretPath != "" && locator.SecretKey != "" {
			return locator.SecretPath, locator.SecretKey, true
		}
	}
	return "", "", false
}

func NormalizeHostname(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if comma := strings.Index(trimmed, ","); comma >= 0 {
		trimmed = strings.TrimSpace(trimmed[:comma])
	}

	if parsed, err := url.Parse(trimmed); err == nil && parsed.Host != "" {
		trimmed = parsed.Host
	}

	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		trimmed = host
	}

	return strings.Trim(strings.ToLower(trimmed), ".")
}

func IsSharedTenantHost(host string) bool {
	normalized := NormalizeHostname(host)
	if normalized == "" {
		return false
	}
	for _, configured := range ConfiguredSharedTenantDomains() {
		if normalized == configured {
			return true
		}
	}
	return false
}

// SharedTenantDomain returns the primary shared tenant domain chosen by the
// deployment. There is intentionally no source-zone fallback: a deployment
// without this configuration must fail closed instead of sharing another
// environment's tenant.
func SharedTenantDomain() string {
	if domain := NormalizeHostname(os.Getenv("XWORKMATE_SHARED_TENANT_DOMAIN")); domain != "" {
		return domain
	}
	domains := ConfiguredSharedTenantDomains()
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}

// ConfiguredSharedTenantDomains returns the deployment-provided shared tenant
// host list. TARGET_DOMAIN_BASE is rendered into this variable by the
// web-saas playbook; keeping the list in configuration avoids coupling tenant
// resolution to a particular public domain.
func ConfiguredSharedTenantDomains() []string {
	seen := make(map[string]struct{})
	domains := make([]string, 0)
	for _, raw := range []string{os.Getenv("XWORKMATE_SHARED_TENANT_DOMAIN"), os.Getenv("XWORKMATE_SHARED_TENANT_DOMAINS")} {
		for _, value := range strings.Split(raw, ",") {
			domain := NormalizeHostname(value)
			if domain == "" {
				continue
			}
			if _, ok := seen[domain]; ok {
				continue
			}
			seen[domain] = struct{}{}
			domains = append(domains, domain)
		}
	}
	return domains
}

func GenerateRandomTenantDomain() (string, error) {
	baseDomain := SharedTenantDomain()
	if baseDomain == "" {
		return "", errors.New("XWORKMATE_SHARED_TENANT_DOMAIN is required to generate a tenant domain")
	}
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate tenant domain: %w", err)
	}
	return fmt.Sprintf("xw-%s.%s", hex.EncodeToString(buffer), baseDomain), nil
}

func NormalizeTenant(tenant *Tenant) {
	if tenant == nil {
		return
	}
	tenant.ID = strings.TrimSpace(tenant.ID)
	tenant.Name = strings.TrimSpace(tenant.Name)
	tenant.Edition = NormalizeTenantEdition(tenant.Edition)
}

func NormalizeTenantDomain(domain *TenantDomain) {
	if domain == nil {
		return
	}
	domain.ID = strings.TrimSpace(domain.ID)
	domain.TenantID = strings.TrimSpace(domain.TenantID)
	domain.Domain = NormalizeHostname(domain.Domain)
	domain.Kind = NormalizeTenantDomainKind(domain.Kind)
	domain.Status = NormalizeTenantDomainStatus(domain.Status)
}

func NormalizeTenantMembership(membership *TenantMembership) {
	if membership == nil {
		return
	}
	membership.TenantID = strings.TrimSpace(membership.TenantID)
	membership.UserID = strings.TrimSpace(membership.UserID)
	membership.Role = NormalizeTenantMembershipRole(membership.Role)
	membership.TenantName = strings.TrimSpace(membership.TenantName)
	membership.TenantEdition = NormalizeTenantEdition(membership.TenantEdition)
	membership.Domain = NormalizeHostname(membership.Domain)
}

func NormalizeXWorkmateProfile(profile *XWorkmateProfile) {
	if profile == nil {
		return
	}
	profile.ID = strings.TrimSpace(profile.ID)
	profile.TenantID = strings.TrimSpace(profile.TenantID)
	profile.UserID = strings.TrimSpace(profile.UserID)
	profile.Scope = NormalizeXWorkmateProfileScope(profile.Scope)
	profile.BridgeServerURL = strings.TrimSpace(profile.BridgeServerURL)
	profile.BridgeServerOrigin = strings.TrimSpace(profile.BridgeServerOrigin)
	profile.VaultURL = strings.TrimSpace(profile.VaultURL)
	profile.VaultNamespace = strings.TrimSpace(profile.VaultNamespace)
	profile.VaultSecretPath = strings.Trim(strings.TrimSpace(profile.VaultSecretPath), "/")
	profile.VaultSecretKey = strings.TrimSpace(profile.VaultSecretKey)
	profile.SecretLocators = cloneXWorkmateSecretLocators(profile.SecretLocators)
	for i := range profile.SecretLocators {
		NormalizeXWorkmateSecretLocator(&profile.SecretLocators[i])
	}
	if len(profile.SecretLocators) == 0 && profile.VaultSecretPath != "" && profile.VaultSecretKey != "" {
		profile.SecretLocators = []XWorkmateSecretLocator{synthesizeXWorkmateSecretLocatorFromLegacy(profile)}
	}
	if (profile.VaultSecretPath == "" || profile.VaultSecretKey == "") && len(profile.SecretLocators) > 0 {
		if secretPath, secretKey, ok := compatibilityXWorkmateSecretLocator(profile.SecretLocators); ok {
			if profile.VaultSecretPath == "" {
				profile.VaultSecretPath = secretPath
			}
			if profile.VaultSecretKey == "" {
				profile.VaultSecretKey = secretKey
			}
		}
	}
	if profile.SecretLocators == nil {
		profile.SecretLocators = []XWorkmateSecretLocator{}
	}
	profile.ApisixURL = strings.TrimSpace(profile.ApisixURL)
}

type TenantResolver interface {
	EnsureTenant(ctx context.Context, tenant *Tenant) error
	EnsureTenantDomain(ctx context.Context, domain *TenantDomain) error
	UpsertTenantMembership(ctx context.Context, membership *TenantMembership) error
	ResolveTenantByHost(ctx context.Context, host string) (*Tenant, *TenantDomain, error)
	ListTenantMembershipsByUser(ctx context.Context, userID string) ([]TenantMembership, error)
	GetTenantMembership(ctx context.Context, tenantID, userID string) (*TenantMembership, error)
	GetXWorkmateProfile(ctx context.Context, tenantID, userID, scope string) (*XWorkmateProfile, error)
	UpsertXWorkmateProfile(ctx context.Context, profile *XWorkmateProfile) error
}
