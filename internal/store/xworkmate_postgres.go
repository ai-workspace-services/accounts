package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

func (s *postgresStore) EnsureTenant(ctx context.Context, tenant *Tenant) error {
	if tenant == nil {
		return ErrTenantNotFound
	}

	NormalizeTenant(tenant)
	if tenant.ID == "" {
		tenant.ID = uuid.NewString()
	}

	query := `INSERT INTO tenants (id, name, edition, created_at, updated_at)
VALUES ($1, $2, $3, now(), now())
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name,
    edition = EXCLUDED.edition,
    updated_at = now()
RETURNING created_at, updated_at`

	return s.db.QueryRowContext(ctx, query, tenant.ID, tenant.Name, tenant.Edition).Scan(&tenant.CreatedAt, &tenant.UpdatedAt)
}

func (s *postgresStore) EnsureTenantDomain(ctx context.Context, domain *TenantDomain) error {
	if domain == nil {
		return ErrTenantNotFound
	}

	NormalizeTenantDomain(domain)
	if domain.ID == "" {
		domain.ID = uuid.NewString()
	}
	if domain.IsPrimary {
		if _, err := s.db.ExecContext(ctx, `UPDATE tenant_domains
SET is_primary = FALSE, updated_at = now()
WHERE tenant_id = $1 AND domain <> $2 AND is_primary = TRUE`, domain.TenantID, domain.Domain); err != nil {
			return err
		}
	}

	query := `INSERT INTO tenant_domains (id, tenant_id, domain, kind, is_primary, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, now(), now())
ON CONFLICT (domain) DO UPDATE
SET tenant_id = EXCLUDED.tenant_id,
    kind = EXCLUDED.kind,
    is_primary = EXCLUDED.is_primary,
    status = EXCLUDED.status,
    updated_at = now()
RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		domain.ID,
		domain.TenantID,
		domain.Domain,
		domain.Kind,
		domain.IsPrimary,
		domain.Status,
	).Scan(&domain.CreatedAt, &domain.UpdatedAt)
}

func (s *postgresStore) UpsertTenantMembership(ctx context.Context, membership *TenantMembership) error {
	if membership == nil {
		return ErrTenantMembershipNotFound
	}

	NormalizeTenantMembership(membership)
	query := `INSERT INTO tenant_memberships (tenant_id, user_id, role, created_at, updated_at)
VALUES ($1, $2, $3, now(), now())
ON CONFLICT (tenant_id, user_id) DO UPDATE
SET role = EXCLUDED.role,
    updated_at = now()
RETURNING created_at, updated_at`

	return s.db.QueryRowContext(ctx, query, membership.TenantID, membership.UserID, membership.Role).Scan(&membership.CreatedAt, &membership.UpdatedAt)
}

func (s *postgresStore) ResolveTenantByHost(ctx context.Context, host string) (*Tenant, *TenantDomain, error) {
	normalizedHost := NormalizeHostname(host)

	if IsSharedTenantHost(normalizedHost) {
		query := `SELECT t.id, t.name, t.edition, t.created_at, t.updated_at,
  COALESCE(td.id, ''), COALESCE(td.domain, ''), COALESCE(td.kind, ''), COALESCE(td.is_primary, false), COALESCE(td.status, ''), td.created_at, td.updated_at
FROM tenants t
LEFT JOIN tenant_domains td
  ON td.tenant_id = t.id AND td.is_primary = TRUE
WHERE t.id = $1
LIMIT 1`
		return scanTenantResolutionRow(s.db.QueryRowContext(ctx, query, SharedXWorkmateTenantID))
	}

	query := `SELECT t.id, t.name, t.edition, t.created_at, t.updated_at,
  td.id, td.domain, td.kind, td.is_primary, td.status, td.created_at, td.updated_at
FROM tenant_domains td
JOIN tenants t ON t.id = td.tenant_id
WHERE td.domain = $1 AND td.status = $2
LIMIT 1`
	return scanTenantResolutionRow(s.db.QueryRowContext(ctx, query, normalizedHost, TenantDomainStatusVerified))
}

func scanTenantResolutionRow(row *sql.Row) (*Tenant, *TenantDomain, error) {
	tenant := &Tenant{}
	var (
		domainID        string
		domainName      string
		domainKind      string
		domainIsPrimary bool
		domainStatus    string
		domainCreatedAt sql.NullTime
		domainUpdatedAt sql.NullTime
	)
	if err := row.Scan(
		&tenant.ID,
		&tenant.Name,
		&tenant.Edition,
		&tenant.CreatedAt,
		&tenant.UpdatedAt,
		&domainID,
		&domainName,
		&domainKind,
		&domainIsPrimary,
		&domainStatus,
		&domainCreatedAt,
		&domainUpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrTenantNotFound
		}
		return nil, nil, err
	}

	var domain *TenantDomain
	if strings.TrimSpace(domainName) != "" {
		domain = &TenantDomain{
			ID:        domainID,
			TenantID:  tenant.ID,
			Domain:    domainName,
			Kind:      domainKind,
			IsPrimary: domainIsPrimary,
			Status:    domainStatus,
		}
		if domainCreatedAt.Valid {
			domain.CreatedAt = domainCreatedAt.Time
		}
		if domainUpdatedAt.Valid {
			domain.UpdatedAt = domainUpdatedAt.Time
		}
	}

	return tenant, domain, nil
}

func (s *postgresStore) ListTenantMembershipsByUser(ctx context.Context, userID string) ([]TenantMembership, error) {
	query := `SELECT tm.tenant_id, tm.user_id, tm.role, tm.created_at, tm.updated_at,
  COALESCE(t.name, ''), COALESCE(t.edition, ''), COALESCE(td.domain, '')
FROM tenant_memberships tm
JOIN tenants t ON t.id = tm.tenant_id
LEFT JOIN tenant_domains td ON td.tenant_id = tm.tenant_id AND td.is_primary = TRUE
WHERE tm.user_id = $1
ORDER BY t.name ASC, tm.tenant_id ASC`

	rows, err := s.db.QueryContext(ctx, query, strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]TenantMembership, 0)
	for rows.Next() {
		var membership TenantMembership
		if err := rows.Scan(
			&membership.TenantID,
			&membership.UserID,
			&membership.Role,
			&membership.CreatedAt,
			&membership.UpdatedAt,
			&membership.TenantName,
			&membership.TenantEdition,
			&membership.Domain,
		); err != nil {
			return nil, err
		}
		result = append(result, membership)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func (s *postgresStore) GetTenantMembership(ctx context.Context, tenantID, userID string) (*TenantMembership, error) {
	query := `SELECT tm.tenant_id, tm.user_id, tm.role, tm.created_at, tm.updated_at,
  COALESCE(t.name, ''), COALESCE(t.edition, ''), COALESCE(td.domain, '')
FROM tenant_memberships tm
JOIN tenants t ON t.id = tm.tenant_id
LEFT JOIN tenant_domains td ON td.tenant_id = tm.tenant_id AND td.is_primary = TRUE
WHERE tm.tenant_id = $1 AND tm.user_id = $2
LIMIT 1`

	membership := &TenantMembership{}
	if err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(tenantID), strings.TrimSpace(userID)).Scan(
		&membership.TenantID,
		&membership.UserID,
		&membership.Role,
		&membership.CreatedAt,
		&membership.UpdatedAt,
		&membership.TenantName,
		&membership.TenantEdition,
		&membership.Domain,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTenantMembershipNotFound
		}
		return nil, err
	}

	return membership, nil
}

func (s *postgresStore) GetXWorkmateProfile(ctx context.Context, tenantID, userID, scope string) (*XWorkmateProfile, error) {
	profile := &XWorkmateProfile{}
	var secretLocatorsJSON string
	query := `SELECT id, tenant_id, user_id, scope, openclaw_url, openclaw_origin, vault_url, vault_namespace, vault_secret_path, vault_secret_key, COALESCE(secret_locators, '[]'), apisix_url, created_at, updated_at
FROM xworkmate_profiles
WHERE tenant_id = $1 AND user_id = $2 AND scope = $3
LIMIT 1`

	if err := s.db.QueryRowContext(
		ctx,
		query,
		strings.TrimSpace(tenantID),
		strings.TrimSpace(userID),
		NormalizeXWorkmateProfileScope(scope),
	).Scan(
		&profile.ID,
		&profile.TenantID,
		&profile.UserID,
		&profile.Scope,
		&profile.BridgeServerURL,
		&profile.BridgeServerOrigin,
		&profile.VaultURL,
		&profile.VaultNamespace,
		&profile.VaultSecretPath,
		&profile.VaultSecretKey,
		&secretLocatorsJSON,
		&profile.ApisixURL,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrXWorkmateProfileNotFound
		}
		return nil, err
	}

	if err := json.Unmarshal([]byte(secretLocatorsJSON), &profile.SecretLocators); err != nil {
		return nil, err
	}
	NormalizeXWorkmateProfile(profile)
	return profile, nil
}

func (s *postgresStore) UpsertXWorkmateProfile(ctx context.Context, profile *XWorkmateProfile) error {
	if profile == nil {
		return ErrXWorkmateProfileNotFound
	}

	NormalizeXWorkmateProfile(profile)
	if profile.ID == "" {
		profile.ID = uuid.NewString()
	}

	locatorsJSON, err := json.Marshal(profile.SecretLocators)
	if err != nil {
		return err
	}

	query := `INSERT INTO xworkmate_profiles (
  id, tenant_id, user_id, scope, openclaw_url, openclaw_origin, vault_url, vault_namespace, vault_secret_path, vault_secret_key, secret_locators, apisix_url, created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), now())
ON CONFLICT (tenant_id, user_id, scope) DO UPDATE
SET openclaw_url = EXCLUDED.openclaw_url,
    openclaw_origin = EXCLUDED.openclaw_origin,
    vault_url = EXCLUDED.vault_url,
    vault_namespace = EXCLUDED.vault_namespace,
    vault_secret_path = EXCLUDED.vault_secret_path,
    vault_secret_key = EXCLUDED.vault_secret_key,
    secret_locators = EXCLUDED.secret_locators,
    apisix_url = EXCLUDED.apisix_url,
    updated_at = now()
RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		profile.ID,
		profile.TenantID,
		profile.UserID,
		profile.Scope,
		profile.BridgeServerURL,
		profile.BridgeServerOrigin,
		profile.VaultURL,
		profile.VaultNamespace,
		profile.VaultSecretPath,
		profile.VaultSecretKey,
		string(locatorsJSON),
		profile.ApisixURL,
	).Scan(&profile.CreatedAt, &profile.UpdatedAt)
}
