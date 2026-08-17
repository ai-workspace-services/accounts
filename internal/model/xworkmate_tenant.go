package model

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Tenant struct {
	ID        string    `gorm:"column:id;type:text;primaryKey"`
	Name      string    `gorm:"column:name;type:text;not null"`
	Edition   string    `gorm:"column:edition;type:text;not null;index"`
	CreatedAt time.Time `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (Tenant) TableName() string { return "tenants" }

func (tenant *Tenant) BeforeCreate(tx *gorm.DB) error {
	if strings.TrimSpace(tenant.ID) == "" {
		tenant.ID = uuid.NewString()
	}
	return nil
}

// TenantDomain uses a column-level UNIQUE constraint rather than a generated
// unique index. Existing PostgreSQL/Supabase databases may name the constraint
// differently (for example, tenant_domains_domain_key). GORM treats
// uniqueIndex as an absent column-level constraint and attempts to drop its
// generated name during AutoMigrate, which makes application startup fail.
type TenantDomain struct {
	ID        string    `gorm:"column:id;type:text;primaryKey"`
	TenantID  string    `gorm:"column:tenant_id;type:text;not null;index"`
	Domain    string    `gorm:"column:domain;type:text;not null;unique"`
	Kind      string    `gorm:"column:kind;type:text;not null"`
	IsPrimary bool      `gorm:"column:is_primary;not null;default:false"`
	Status    string    `gorm:"column:status;type:text;not null;index"`
	CreatedAt time.Time `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (TenantDomain) TableName() string { return "tenant_domains" }

func (domain *TenantDomain) BeforeCreate(tx *gorm.DB) error {
	if strings.TrimSpace(domain.ID) == "" {
		domain.ID = uuid.NewString()
	}
	return nil
}

type TenantMembership struct {
	TenantID  string    `gorm:"column:tenant_id;type:text;primaryKey"`
	UserID    string    `gorm:"column:user_id;type:text;primaryKey"`
	Role      string    `gorm:"column:role;type:text;not null;index"`
	CreatedAt time.Time `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (TenantMembership) TableName() string { return "tenant_memberships" }

type XWorkmateProfile struct {
	ID                 string                   `gorm:"column:id;type:text;primaryKey"`
	TenantID           string                   `gorm:"column:tenant_id;type:text;not null;uniqueIndex:idx_xworkmate_profiles_scope"`
	UserID             string                   `gorm:"column:user_id;type:text;not null;default:'';uniqueIndex:idx_xworkmate_profiles_scope"`
	Scope              string                   `gorm:"column:scope;type:text;not null;uniqueIndex:idx_xworkmate_profiles_scope"`
	BridgeServerURL    string                   `gorm:"column:openclaw_url;type:text;not null;default:''"`
	BridgeServerOrigin string                   `gorm:"column:openclaw_origin;type:text;not null;default:''"`
	VaultURL           string                   `gorm:"column:vault_url;type:text;not null;default:''"`
	VaultNamespace     string                   `gorm:"column:vault_namespace;type:text;not null;default:''"`
	VaultSecretPath    string                   `gorm:"column:vault_secret_path;type:text;not null;default:''"`
	VaultSecretKey     string                   `gorm:"column:vault_secret_key;type:text;not null;default:''"`
	SecretLocators     []XWorkmateSecretLocator `gorm:"column:secret_locators;type:text;not null;serializer:json;default:'[]'"`
	ApisixURL          string                   `gorm:"column:apisix_url;type:text;not null;default:''"`
	CreatedAt          time.Time                `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt          time.Time                `gorm:"column:updated_at;not null;autoUpdateTime"`
}

type XWorkmateSecretLocator struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	SecretPath string `json:"secretPath"`
	SecretKey  string `json:"secretKey"`
	Target     string `json:"target"`
	Required   bool   `json:"required"`
}

func (XWorkmateProfile) TableName() string { return "xworkmate_profiles" }

func (profile *XWorkmateProfile) BeforeCreate(tx *gorm.DB) error {
	if strings.TrimSpace(profile.ID) == "" {
		profile.ID = uuid.NewString()
	}
	return nil
}
