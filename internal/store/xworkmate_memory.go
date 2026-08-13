package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func tenantProfileKey(tenantID, userID, scope string) string {
	return strings.Join([]string{
		strings.TrimSpace(tenantID),
		strings.TrimSpace(userID),
		NormalizeXWorkmateProfileScope(scope),
	}, "|")
}

func (s *memoryStore) EnsureTenant(ctx context.Context, tenant *Tenant) error {
	_ = ctx
	if tenant == nil {
		return ErrTenantNotFound
	}

	NormalizeTenant(tenant)
	if tenant.ID == "" {
		tenant.ID = uuid.NewString()
	}

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.tenants[tenant.ID]
	if ok {
		existing.Name = tenant.Name
		existing.Edition = tenant.Edition
		existing.UpdatedAt = now
		tenant.CreatedAt = existing.CreatedAt
		tenant.UpdatedAt = existing.UpdatedAt
		return nil
	}

	stored := &Tenant{
		ID:        tenant.ID,
		Name:      tenant.Name,
		Edition:   tenant.Edition,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tenants[stored.ID] = stored
	tenant.CreatedAt = stored.CreatedAt
	tenant.UpdatedAt = stored.UpdatedAt
	return nil
}

func (s *memoryStore) EnsureTenantDomain(ctx context.Context, domain *TenantDomain) error {
	_ = ctx
	if domain == nil {
		return ErrTenantNotFound
	}

	NormalizeTenantDomain(domain)
	if domain.Domain == "" || domain.TenantID == "" {
		return ErrTenantNotFound
	}
	if domain.ID == "" {
		domain.ID = uuid.NewString()
	}

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.tenantDomains[domain.Domain]
	if ok {
		existing.TenantID = domain.TenantID
		existing.Kind = domain.Kind
		existing.IsPrimary = domain.IsPrimary
		existing.Status = domain.Status
		existing.UpdatedAt = now
		domain.CreatedAt = existing.CreatedAt
		domain.UpdatedAt = existing.UpdatedAt
		return nil
	}
	if domain.IsPrimary {
		for _, existingDomain := range s.tenantDomains {
			if existingDomain.TenantID == domain.TenantID && existingDomain.Domain != domain.Domain {
				existingDomain.IsPrimary = false
			}
		}
	}

	stored := &TenantDomain{
		ID:        domain.ID,
		TenantID:  domain.TenantID,
		Domain:    domain.Domain,
		Kind:      domain.Kind,
		IsPrimary: domain.IsPrimary,
		Status:    domain.Status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tenantDomains[stored.Domain] = stored
	domain.CreatedAt = stored.CreatedAt
	domain.UpdatedAt = stored.UpdatedAt
	return nil
}

func (s *memoryStore) UpsertTenantMembership(ctx context.Context, membership *TenantMembership) error {
	_ = ctx
	if membership == nil {
		return ErrTenantMembershipNotFound
	}

	NormalizeTenantMembership(membership)
	if membership.TenantID == "" || membership.UserID == "" {
		return ErrTenantMembershipNotFound
	}

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tenantMemberships[membership.TenantID] == nil {
		s.tenantMemberships[membership.TenantID] = make(map[string]*TenantMembership)
	}

	if existing, ok := s.tenantMemberships[membership.TenantID][membership.UserID]; ok {
		existing.Role = membership.Role
		existing.UpdatedAt = now
		membership.CreatedAt = existing.CreatedAt
		membership.UpdatedAt = existing.UpdatedAt
		return nil
	}

	stored := &TenantMembership{
		TenantID:  membership.TenantID,
		UserID:    membership.UserID,
		Role:      membership.Role,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tenantMemberships[membership.TenantID][membership.UserID] = stored
	membership.CreatedAt = stored.CreatedAt
	membership.UpdatedAt = stored.UpdatedAt
	return nil
}

func (s *memoryStore) ResolveTenantByHost(ctx context.Context, host string) (*Tenant, *TenantDomain, error) {
	_ = ctx
	normalizedHost := NormalizeHostname(host)

	s.mu.RLock()
	defer s.mu.RUnlock()

	if IsSharedTenantHost(normalizedHost) {
		tenant, ok := s.tenants[SharedXWorkmateTenantID]
		if !ok {
			return nil, nil, ErrTenantNotFound
		}
		var domain *TenantDomain
		if storedDomain, ok := s.tenantDomains[SharedTenantDomain()]; ok {
			domainCopy := *storedDomain
			domain = &domainCopy
		}
		tenantCopy := *tenant
		return &tenantCopy, domain, nil
	}

	domain, ok := s.tenantDomains[normalizedHost]
	if !ok {
		return nil, nil, ErrTenantNotFound
	}
	tenant, ok := s.tenants[domain.TenantID]
	if !ok {
		return nil, nil, ErrTenantNotFound
	}

	tenantCopy := *tenant
	domainCopy := *domain
	return &tenantCopy, &domainCopy, nil
}

func (s *memoryStore) ListTenantMembershipsByUser(ctx context.Context, userID string) ([]TenantMembership, error) {
	_ = ctx
	normalizedUserID := strings.TrimSpace(userID)

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]TenantMembership, 0)
	for tenantID, members := range s.tenantMemberships {
		member, ok := members[normalizedUserID]
		if !ok {
			continue
		}

		entry := *member
		if tenant, ok := s.tenants[tenantID]; ok {
			entry.TenantName = tenant.Name
			entry.TenantEdition = tenant.Edition
		}
		for _, domain := range s.tenantDomains {
			if domain.TenantID == tenantID && domain.IsPrimary {
				entry.Domain = domain.Domain
				break
			}
		}
		result = append(result, entry)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].TenantName == result[j].TenantName {
			return result[i].TenantID < result[j].TenantID
		}
		return result[i].TenantName < result[j].TenantName
	})

	return result, nil
}

func (s *memoryStore) GetTenantMembership(ctx context.Context, tenantID, userID string) (*TenantMembership, error) {
	_ = ctx
	normalizedTenantID := strings.TrimSpace(tenantID)
	normalizedUserID := strings.TrimSpace(userID)

	s.mu.RLock()
	defer s.mu.RUnlock()

	members := s.tenantMemberships[normalizedTenantID]
	if members == nil {
		return nil, ErrTenantMembershipNotFound
	}
	member, ok := members[normalizedUserID]
	if !ok {
		return nil, ErrTenantMembershipNotFound
	}

	entry := *member
	if tenant, ok := s.tenants[normalizedTenantID]; ok {
		entry.TenantName = tenant.Name
		entry.TenantEdition = tenant.Edition
	}
	for _, domain := range s.tenantDomains {
		if domain.TenantID == normalizedTenantID && domain.IsPrimary {
			entry.Domain = domain.Domain
			break
		}
	}
	return &entry, nil
}

func (s *memoryStore) GetXWorkmateProfile(ctx context.Context, tenantID, userID, scope string) (*XWorkmateProfile, error) {
	_ = ctx
	key := tenantProfileKey(tenantID, userID, scope)

	s.mu.RLock()
	defer s.mu.RUnlock()

	profile, ok := s.xworkmateProfiles[key]
	if !ok {
		return nil, ErrXWorkmateProfileNotFound
	}

	entry := *profile
	entry.SecretLocators = cloneXWorkmateSecretLocators(entry.SecretLocators)
	NormalizeXWorkmateProfile(&entry)
	return &entry, nil
}

func (s *memoryStore) UpsertXWorkmateProfile(ctx context.Context, profile *XWorkmateProfile) error {
	_ = ctx
	if profile == nil {
		return ErrXWorkmateProfileNotFound
	}

	NormalizeXWorkmateProfile(profile)
	if profile.TenantID == "" {
		return ErrXWorkmateProfileNotFound
	}
	if profile.Scope == XWorkmateProfileScopeUserPrivate && profile.UserID == "" {
		return ErrXWorkmateProfileNotFound
	}
	if profile.Scope == XWorkmateProfileScopeTenantShared {
		profile.UserID = ""
	}
	if profile.ID == "" {
		profile.ID = uuid.NewString()
	}

	now := time.Now().UTC()
	key := tenantProfileKey(profile.TenantID, profile.UserID, profile.Scope)

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.xworkmateProfiles[key]; ok {
		existing.BridgeServerURL = profile.BridgeServerURL
		existing.BridgeServerOrigin = profile.BridgeServerOrigin
		existing.VaultURL = profile.VaultURL
		existing.VaultNamespace = profile.VaultNamespace
		existing.VaultSecretPath = profile.VaultSecretPath
		existing.VaultSecretKey = profile.VaultSecretKey
		existing.SecretLocators = cloneXWorkmateSecretLocators(profile.SecretLocators)
		existing.ApisixURL = profile.ApisixURL
		existing.UpdatedAt = now
		profile.CreatedAt = existing.CreatedAt
		profile.UpdatedAt = existing.UpdatedAt
		return nil
	}

	stored := &XWorkmateProfile{
		ID:                 profile.ID,
		TenantID:           profile.TenantID,
		UserID:             profile.UserID,
		Scope:              profile.Scope,
		BridgeServerURL:    profile.BridgeServerURL,
		BridgeServerOrigin: profile.BridgeServerOrigin,
		VaultURL:           profile.VaultURL,
		VaultNamespace:     profile.VaultNamespace,
		VaultSecretPath:    profile.VaultSecretPath,
		VaultSecretKey:     profile.VaultSecretKey,
		SecretLocators:     cloneXWorkmateSecretLocators(profile.SecretLocators),
		ApisixURL:          profile.ApisixURL,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	s.xworkmateProfiles[key] = stored
	profile.CreatedAt = stored.CreatedAt
	profile.UpdatedAt = stored.UpdatedAt
	return nil
}
