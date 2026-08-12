package store

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeHostname(t *testing.T) {
	t.Parallel()

	got := NormalizeHostname("https://XW-ABCD.svc.plus:443/path?q=1")
	if got != "xw-abcd.svc.plus" {
		t.Fatalf("expected normalized host, got %q", got)
	}
}

func TestIsSharedTenantHostUsesDeploymentConfiguredDomains(t *testing.T) {
	t.Setenv("XWORKMATE_SHARED_TENANT_DOMAINS", "onwalk.net, console-uat.onwalk.net, onwalk.net")

	if !IsSharedTenantHost("https://onwalk.net") {
		t.Fatal("expected configured target domain to resolve to the shared tenant")
	}
	if !IsSharedTenantHost("console-uat.onwalk.net") {
		t.Fatal("expected configured environment alias to resolve to the shared tenant")
	}
	if IsSharedTenantHost("private.example.net") {
		t.Fatal("unexpectedly treated an unconfigured private host as shared")
	}
	got := ConfiguredSharedTenantDomains()
	if len(got) != 2 || got[0] != "onwalk.net" || got[1] != "console-uat.onwalk.net" {
		t.Fatalf("unexpected configured domains: %#v", got)
	}
}

func TestGenerateRandomTenantDomain(t *testing.T) {
	t.Parallel()

	got, err := GenerateRandomTenantDomain()
	if err != nil {
		t.Fatalf("expected domain generation to succeed: %v", err)
	}
	if !strings.HasPrefix(got, "xw-") || !strings.HasSuffix(got, ".svc.plus") {
		t.Fatalf("expected generated svc.plus tenant domain, got %q", got)
	}
}

func TestMemoryStoreResolveTenantAndProfile(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()

	if err := st.EnsureTenant(ctx, &Tenant{
		ID:      SharedXWorkmateTenantID,
		Name:    SharedXWorkmateTenantName,
		Edition: SharedPublicTenantEdition,
	}); err != nil {
		t.Fatalf("ensure shared tenant: %v", err)
	}
	if err := st.EnsureTenantDomain(ctx, &TenantDomain{
		TenantID:  SharedXWorkmateTenantID,
		Domain:    SharedXWorkmateDomain,
		Kind:      TenantDomainKindGenerated,
		IsPrimary: true,
		Status:    TenantDomainStatusVerified,
	}); err != nil {
		t.Fatalf("ensure shared domain: %v", err)
	}

	tenant, domain, err := st.ResolveTenantByHost(ctx, "console.svc.plus")
	if err != nil {
		t.Fatalf("resolve shared tenant: %v", err)
	}
	if tenant.ID != SharedXWorkmateTenantID {
		t.Fatalf("expected shared tenant id, got %q", tenant.ID)
	}
	if domain == nil || domain.Domain != SharedXWorkmateDomain {
		t.Fatalf("expected shared primary domain, got %#v", domain)
	}

	privateTenant := &Tenant{
		ID:      "tenant-private-1",
		Name:    "Tenant One",
		Edition: TenantPrivateEdition,
	}
	if err := st.EnsureTenant(ctx, privateTenant); err != nil {
		t.Fatalf("ensure private tenant: %v", err)
	}
	if err := st.EnsureTenantDomain(ctx, &TenantDomain{
		TenantID:  privateTenant.ID,
		Domain:    "xw-tenant-one.svc.plus",
		Kind:      TenantDomainKindGenerated,
		IsPrimary: true,
		Status:    TenantDomainStatusVerified,
	}); err != nil {
		t.Fatalf("ensure private domain: %v", err)
	}
	if err := st.UpsertTenantMembership(ctx, &TenantMembership{
		TenantID: privateTenant.ID,
		UserID:   "user-1",
		Role:     TenantMembershipRoleAdmin,
	}); err != nil {
		t.Fatalf("ensure private membership: %v", err)
	}
	if err := st.UpsertXWorkmateProfile(ctx, &XWorkmateProfile{
		TenantID:        privateTenant.ID,
		UserID:          "user-1",
		Scope:           XWorkmateProfileScopeUserPrivate,
		BridgeServerURL: "wss://openclaw.tenant-one.svc.plus",
		VaultSecretPath: "kv/openclaw",
		VaultSecretKey:  "token",
	}); err != nil {
		t.Fatalf("upsert private profile: %v", err)
	}

	tenant, domain, err = st.ResolveTenantByHost(ctx, "https://xw-tenant-one.svc.plus")
	if err != nil {
		t.Fatalf("resolve private tenant: %v", err)
	}
	if tenant.ID != privateTenant.ID {
		t.Fatalf("expected tenant %q, got %q", privateTenant.ID, tenant.ID)
	}
	if domain == nil || domain.Domain != "xw-tenant-one.svc.plus" {
		t.Fatalf("expected tenant domain, got %#v", domain)
	}

	profile, err := st.GetXWorkmateProfile(ctx, privateTenant.ID, "user-1", XWorkmateProfileScopeUserPrivate)
	if err != nil {
		t.Fatalf("get private profile: %v", err)
	}
	if profile.BridgeServerURL != "wss://openclaw.tenant-one.svc.plus" {
		t.Fatalf("expected persisted bridge server url, got %q", profile.BridgeServerURL)
	}
	if profile.VaultSecretPath != "kv/openclaw" || profile.VaultSecretKey != "token" {
		t.Fatalf("expected legacy secret fields to round-trip, got %#v", profile)
	}
	if len(profile.SecretLocators) != 1 {
		t.Fatalf("expected synthesized secret locator, got %#v", profile.SecretLocators)
	}
	if profile.SecretLocators[0].Provider != XWorkmateSecretLocatorProviderVault {
		t.Fatalf("expected vault provider, got %#v", profile.SecretLocators[0])
	}
	if profile.SecretLocators[0].Target != XWorkmateSecretLocatorTargetBridgeAuthToken {
		t.Fatalf("expected bridge auth token target, got %#v", profile.SecretLocators[0])
	}
	if profile.SecretLocators[0].SecretPath != "kv/openclaw" || profile.SecretLocators[0].SecretKey != "token" {
		t.Fatalf("expected synthesized secret locator path/key, got %#v", profile.SecretLocators[0])
	}

	memberships, err := st.ListTenantMembershipsByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships) != 1 {
		t.Fatalf("expected 1 tenant membership, got %d", len(memberships))
	}
	if memberships[0].TenantName != "Tenant One" {
		t.Fatalf("expected tenant name to be populated, got %q", memberships[0].TenantName)
	}
}

func TestMemoryStorePersistsExplicitSecretLocators(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()

	if err := st.EnsureTenant(ctx, &Tenant{
		ID:      "tenant-locator-1",
		Name:    "Tenant Locator",
		Edition: TenantPrivateEdition,
	}); err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}

	locators := []XWorkmateSecretLocator{
		{
			ID:         "locator-openclaw",
			Provider:   "vault",
			SecretPath: "kv/openclaw",
			SecretKey:  "gateway-token",
			Target:     XWorkmateSecretLocatorTargetBridgeAuthToken,
			Required:   true,
		},
		{
			ID:         "locator-ai-gateway",
			Provider:   "vault",
			SecretPath: "kv/ai",
			SecretKey:  "access-token",
			Target:     XWorkmateSecretLocatorTargetAIGatewayAccessToken,
		},
	}
	if err := st.UpsertXWorkmateProfile(ctx, &XWorkmateProfile{
		TenantID:       "tenant-locator-1",
		UserID:         "user-2",
		Scope:          XWorkmateProfileScopeUserPrivate,
		VaultURL:       "https://vault.example.com",
		VaultNamespace: "team-locators",
		SecretLocators: locators,
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	profile, err := st.GetXWorkmateProfile(ctx, "tenant-locator-1", "user-2", XWorkmateProfileScopeUserPrivate)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}

	if len(profile.SecretLocators) != len(locators) {
		t.Fatalf("expected %d locators, got %#v", len(locators), profile.SecretLocators)
	}
	for i := range locators {
		if profile.SecretLocators[i].ID != locators[i].ID ||
			profile.SecretLocators[i].Provider != locators[i].Provider ||
			profile.SecretLocators[i].SecretPath != locators[i].SecretPath ||
			profile.SecretLocators[i].SecretKey != locators[i].SecretKey ||
			profile.SecretLocators[i].Target != locators[i].Target ||
			profile.SecretLocators[i].Required != locators[i].Required {
			t.Fatalf("locator %d mismatch: got %#v want %#v", i, profile.SecretLocators[i], locators[i])
		}
	}
	if profile.VaultSecretPath != "kv/openclaw" || profile.VaultSecretKey != "gateway-token" {
		t.Fatalf("expected openclaw locator to back legacy fields, got %#v", profile)
	}
}
