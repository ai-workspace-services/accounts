package overlay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The repository deliberately stores only token digests. Raw join tokens,
// device credentials, enrollment bearers, and client private keys never enter
// these models.
type NetworkRecord struct {
	ID                      string    `gorm:"column:id;type:text;primaryKey"`
	DisplayName             string    `gorm:"column:display_name;type:text;not null"`
	CIDR                    string    `gorm:"column:cidr;type:text;not null"`
	GatewayID               string    `gorm:"column:gateway_id;type:text;not null"`
	GatewayWireGuardKey     string    `gorm:"column:gateway_wireguard_key;type:text;not null"`
	GatewayWireGuardAddress string    `gorm:"column:gateway_wireguard_address;type:text;not null;default:''"`
	GatewayEndpointHost     string    `gorm:"column:gateway_endpoint_host;type:text;not null"`
	GatewayEndpointPort     int       `gorm:"column:gateway_endpoint_port;not null"`
	TransportServerName     string    `gorm:"column:transport_server_name;type:text;not null"`
	TransportPort           int       `gorm:"column:transport_port;not null"`
	TransportAuthID         string    `gorm:"column:transport_auth_id;type:text;not null"`
	OwnerUserID             string    `gorm:"column:owner_user_id;type:text;index"`
	PolicyJSON              string    `gorm:"column:policy_json;type:text;not null;default:''"`
	ConfigGeneration        uint64    `gorm:"column:config_generation;not null;default:1"`
	CreatedAt               time.Time `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt               time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (NetworkRecord) TableName() string { return "overlay_networks" }

type InviteRecord struct {
	ID            string     `gorm:"column:id;type:text;primaryKey"`
	NetworkID     string     `gorm:"column:network_id;type:text;not null;index"`
	TokenHash     string     `gorm:"column:token_hash;type:text;not null;uniqueIndex"`
	DeviceID      string     `gorm:"column:device_id;type:text"`
	Platform      string     `gorm:"column:platform;type:text"`
	Role          string     `gorm:"column:role;type:text;not null;default:'one'"`
	ExpiresAt     time.Time  `gorm:"column:expires_at;not null;index"`
	RemainingUses int        `gorm:"column:remaining_uses;not null"`
	ConsumedAt    *time.Time `gorm:"column:consumed_at"`
	CreatedAt     time.Time  `gorm:"column:created_at;not null;autoCreateTime"`
}

func (InviteRecord) TableName() string { return "overlay_invites" }

type DeviceRecord struct {
	ID                 string     `gorm:"column:id;type:text;primaryKey"`
	UserID             string     `gorm:"column:user_id;type:text;index"`
	NetworkID          string     `gorm:"column:network_id;type:text;not null;index"`
	Role               string     `gorm:"column:role;type:text;not null;default:'one';index"`
	Name               string     `gorm:"column:name;type:text;not null"`
	Platform           string     `gorm:"column:platform;type:text;not null"`
	Hostname           string     `gorm:"column:hostname;type:text;not null"`
	WireGuardPublicKey string     `gorm:"column:wireguard_public_key;type:text;not null"`
	WireGuardAddress   string     `gorm:"column:wireguard_address;type:text;not null"`
	Status             string     `gorm:"column:status;type:text;not null;index"`
	LastSeenAt         *time.Time `gorm:"column:last_seen_at"`
	CreatedAt          time.Time  `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (DeviceRecord) TableName() string { return "overlay_devices" }

type CredentialRecord struct {
	ID           string     `gorm:"column:id;type:text;primaryKey"`
	DeviceID     string     `gorm:"column:device_id;type:text;not null;index"`
	CredentialID string     `gorm:"column:credential_id;type:text;not null;uniqueIndex"`
	TokenHash    string     `gorm:"column:token_hash;type:text;not null;uniqueIndex"`
	IssuedAt     time.Time  `gorm:"column:issued_at;not null"`
	ExpiresAt    time.Time  `gorm:"column:expires_at;not null;index"`
	RevokedAt    *time.Time `gorm:"column:revoked_at"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null;autoCreateTime"`
}

func (CredentialRecord) TableName() string { return "overlay_device_credentials" }

type EnrollmentRecord struct {
	ID         string     `gorm:"column:id;type:text;primaryKey"`
	DeviceID   string     `gorm:"column:device_id;type:text;not null;index"`
	TokenHash  string     `gorm:"column:token_hash;type:text;not null;uniqueIndex"`
	IssuedAt   time.Time  `gorm:"column:issued_at;not null"`
	ExpiresAt  time.Time  `gorm:"column:expires_at;not null;index"`
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
	CreatedAt  time.Time  `gorm:"column:created_at;not null;autoCreateTime"`
}

func (EnrollmentRecord) TableName() string { return "overlay_enrollment_sessions" }

type AckRecord struct {
	ID         string    `gorm:"column:id;type:text;primaryKey"`
	DeviceID   string    `gorm:"column:device_id;type:text;not null;index"`
	NetworkID  string    `gorm:"column:network_id;type:text;not null;index"`
	Generation uint64    `gorm:"column:generation;not null"`
	ConfigID   string    `gorm:"column:config_id;type:text;not null"`
	AppliedAt  time.Time `gorm:"column:applied_at;not null"`
	ReceivedAt time.Time `gorm:"column:received_at;not null"`
}

func (AckRecord) TableName() string { return "overlay_signed_config_acks" }

type Repository struct{ DB *gorm.DB }

func NewRepository(db *gorm.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("overlay repository requires a database")
	}
	return &Repository{DB: db}, nil
}

func AutoMigrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("overlay migration requires a database")
	}
	return db.AutoMigrate(&NetworkRecord{}, &InviteRecord{}, &DeviceRecord{}, &CredentialRecord{}, &EnrollmentRecord{}, &AckRecord{})
}

func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (r *Repository) Seed(ctx context.Context, cfg BootstrapConfig, joinToken string) (string, error) {
	if err := cfg.Network.validate(); err != nil {
		return "", err
	}
	if cfg.Invite.ExpiresAt.IsZero() || !cfg.Invite.ExpiresAt.After(time.Now().UTC()) {
		return "", ErrInvalidInput
	}
	role := cfg.Invite.Role
	if role == "" {
		role = RoleOne
	}
	if role != RoleOne && role != RoleGateway {
		return "", ErrInvalidInput
	}
	joinToken = strings.TrimSpace(joinToken)
	if joinToken == "" {
		joinToken = newOpaqueToken("xjt_")
	}
	if err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		network := NetworkRecord{
			ID: cfg.Network.ID, DisplayName: cfg.Network.DisplayName, CIDR: cfg.Network.CIDR,
			GatewayID: cfg.Network.GatewayID, GatewayWireGuardKey: cfg.Network.GatewayWireGuardKey,
			GatewayWireGuardAddress: cfg.Network.GatewayWireGuardAddress,
			GatewayEndpointHost:     cfg.Network.GatewayEndpointHost, GatewayEndpointPort: cfg.Network.GatewayEndpointPort,
			TransportServerName: cfg.Network.TransportServerName, TransportPort: cfg.Network.TransportPort,
			TransportAuthID: cfg.Network.TransportAuthID, OwnerUserID: cfg.Network.OwnerUserID, PolicyJSON: "",
			ConfigGeneration: 1,
		}
		var existing NetworkRecord
		if err := tx.Where("id = ?", network.ID).First(&existing).Error; err == nil {
			if strings.TrimSpace(existing.OwnerUserID) != strings.TrimSpace(network.OwnerUserID) {
				return ErrForbidden
			}
			if err := tx.Model(&NetworkRecord{}).Where("id = ?", network.ID).Updates(map[string]any{
				"display_name": network.DisplayName, "cidr": network.CIDR, "gateway_id": network.GatewayID,
				"gateway_wireguard_key": network.GatewayWireGuardKey, "gateway_wireguard_address": network.GatewayWireGuardAddress, "gateway_endpoint_host": network.GatewayEndpointHost,
				"gateway_endpoint_port": network.GatewayEndpointPort, "transport_server_name": network.TransportServerName,
				"transport_port": network.TransportPort, "transport_auth_id": network.TransportAuthID,
				"owner_user_id": network.OwnerUserID,
			}).Error; err != nil {
				return err
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		} else if err := tx.Create(&network).Error; err != nil {
			return err
		}
		invite := InviteRecord{ID: uuid.NewString(), NetworkID: cfg.Network.ID, TokenHash: HashSecret(joinToken), DeviceID: cfg.Invite.DeviceID, Platform: cfg.Invite.Platform, Role: role, ExpiresAt: cfg.Invite.ExpiresAt.UTC().Truncate(time.Second), RemainingUses: 1}
		return tx.Where("token_hash = ?", invite.TokenHash).FirstOrCreate(&invite).Error
	}); err != nil {
		return "", err
	}
	return joinToken, nil
}

func (r *Repository) peekInvite(ctx context.Context, tokenHash string) (InviteRecord, error) {
	var invite InviteRecord
	if err := r.DB.WithContext(ctx).Where("token_hash = ?", tokenHash).First(&invite).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return invite, ErrInvalidToken
		}
		return invite, err
	}
	return invite, nil
}

func (r *Repository) network(ctx context.Context, id string) (NetworkRecord, error) {
	var record NetworkRecord
	err := r.DB.WithContext(ctx).Where("id = ?", id).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return record, ErrNotFound
	}
	return record, err
}

func (r *Repository) allocateAddress(tx *gorm.DB, network NetworkRecord) (string, error) {
	prefix, err := netip.ParsePrefix(network.CIDR)
	if err != nil {
		return "", ErrInvalidInput
	}
	gatewayAddress := network.GatewayWireGuardAddress
	if gatewayAddress == "" {
		gatewayAddress = prefix.Addr().Next().String() + "/32"
	}
	for candidate := prefix.Addr(); prefix.Contains(candidate); candidate = candidate.Next() {
		if candidate == prefix.Addr() {
			continue
		}
		address := candidate.String() + "/32"
		if address == gatewayAddress {
			continue
		}
		var count int64
		if err := tx.Model(&DeviceRecord{}).Where("network_id = ? AND wireguard_address = ?", network.ID, address).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return address, nil
		}
	}
	return "", errors.New("overlay network address pool exhausted")
}

func (r *Repository) createDevice(ctx context.Context, tokenHash string, request ExchangeRequest, now time.Time, credential CredentialRecord, enrollment EnrollmentRecord) (InviteRecord, DeviceRecord, NetworkRecord, error) {
	var invite InviteRecord
	var device DeviceRecord
	var network NetworkRecord
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&InviteRecord{}).Where("token_hash = ? AND remaining_uses > 0 AND expires_at > ? AND consumed_at IS NULL", tokenHash, now).Updates(map[string]any{"remaining_uses": 0, "consumed_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInvalidToken
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", tokenHash).First(&invite).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ? AND network_id = ?", request.DeviceID, invite.NetworkID).First(&DeviceRecord{}).Error; err == nil {
			return ErrDeviceConflict
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var networkErr error
		network, networkErr = r.networkTx(tx, invite.NetworkID)
		if networkErr != nil {
			return networkErr
		}
		address, err := r.allocateAddress(tx, network)
		if err != nil {
			return err
		}
		device = DeviceRecord{ID: request.DeviceID, UserID: network.OwnerUserID, NetworkID: invite.NetworkID, Role: invite.Role, Name: request.Name, Platform: request.Platform, Hostname: request.Hostname, WireGuardPublicKey: request.WireGuardPublicKey, WireGuardAddress: address, Status: "active", CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&device).Error; err != nil {
			return err
		}
		if err := tx.Model(&NetworkRecord{}).Where("id = ?", network.ID).UpdateColumn("config_generation", gorm.Expr("config_generation + 1")).Error; err != nil {
			return err
		}
		network.ConfigGeneration++
		credential.DeviceID = device.ID
		enrollment.DeviceID = device.ID
		if err := tx.Create(&credential).Error; err != nil {
			return err
		}
		return tx.Create(&enrollment).Error
	})
	return invite, device, network, err
}

func (r *Repository) networkTx(tx *gorm.DB, id string) (NetworkRecord, error) {
	var network NetworkRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&network).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return network, ErrNotFound
		}
		return network, err
	}
	return network, nil
}

func (r *Repository) credential(ctx context.Context, hash string) (CredentialRecord, DeviceRecord, NetworkRecord, error) {
	var credential CredentialRecord
	if err := r.DB.WithContext(ctx).Where("token_hash = ?", hash).First(&credential).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return credential, DeviceRecord{}, NetworkRecord{}, ErrInvalidToken
		}
		return credential, DeviceRecord{}, NetworkRecord{}, err
	}
	var device DeviceRecord
	if err := r.DB.WithContext(ctx).Where("id = ?", credential.DeviceID).First(&device).Error; err != nil {
		return credential, device, NetworkRecord{}, err
	}
	network, err := r.network(ctx, device.NetworkID)
	return credential, device, network, err
}

func (r *Repository) enrollment(ctx context.Context, hash string) (EnrollmentRecord, DeviceRecord, NetworkRecord, error) {
	var enrollment EnrollmentRecord
	if err := r.DB.WithContext(ctx).Where("token_hash = ?", hash).First(&enrollment).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return enrollment, DeviceRecord{}, NetworkRecord{}, ErrInvalidToken
		}
		return enrollment, DeviceRecord{}, NetworkRecord{}, err
	}
	var device DeviceRecord
	if err := r.DB.WithContext(ctx).Where("id = ?", enrollment.DeviceID).First(&device).Error; err != nil {
		return enrollment, device, NetworkRecord{}, err
	}
	network, err := r.network(ctx, device.NetworkID)
	return enrollment, device, network, err
}

func (r *Repository) userDevice(ctx context.Context, userID, deviceID string) (DeviceRecord, NetworkRecord, error) {
	var device DeviceRecord
	query := r.DB.WithContext(ctx).Where("id = ? AND user_id = ? AND status = ?", deviceID, userID, "active")
	if err := query.First(&device).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return device, NetworkRecord{}, ErrForbidden
		}
		return device, NetworkRecord{}, err
	}
	network, err := r.network(ctx, device.NetworkID)
	return device, network, err
}

func (r *Repository) devices(ctx context.Context, networkID string) ([]DeviceRecord, error) {
	var devices []DeviceRecord
	if err := r.DB.WithContext(ctx).Where("network_id = ? AND status = ?", networkID, "active").Order("id ASC").Find(&devices).Error; err != nil {
		return nil, err
	}
	return devices, nil
}

func (r *Repository) ack(ctx context.Context, request SignedConfigAckRequest, device DeviceRecord, network NetworkRecord, now time.Time) (SignedConfigAckResponse, error) {
	if request.Generation != network.ConfigGeneration {
		return SignedConfigAckResponse{}, ErrGenerationConflict
	}
	var existing AckRecord
	err := r.DB.WithContext(ctx).Where("device_id = ? AND generation = ?", device.ID, request.Generation).First(&existing).Error
	if err == nil {
		if existing.ConfigID != request.ConfigID || existing.DeviceID != request.DeviceID {
			return SignedConfigAckResponse{}, ErrGenerationConflict
		}
		return ackResponse(existing, true), nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return SignedConfigAckResponse{}, err
	}
	record := AckRecord{ID: uuid.NewString(), DeviceID: device.ID, NetworkID: network.ID, Generation: request.Generation, ConfigID: request.ConfigID, AppliedAt: request.AppliedAt.UTC().Truncate(time.Second), ReceivedAt: now.UTC().Truncate(time.Second)}
	if err := r.DB.WithContext(ctx).Create(&record).Error; err != nil {
		return SignedConfigAckResponse{}, err
	}
	return ackResponse(record, false), nil
}

func ackResponse(record AckRecord, duplicate bool) SignedConfigAckResponse {
	return SignedConfigAckResponse{Acked: true, Duplicate: duplicate, Ack: SignedConfigAck{DeviceID: record.DeviceID, ConfigID: record.ConfigID, Generation: record.Generation, AppliedAt: record.AppliedAt.UTC(), ReceivedAt: record.ReceivedAt.UTC()}}
}

func (r *Repository) ackUser(ctx context.Context, request SignedConfigAckRequest, userID string, network NetworkRecord, now time.Time) (SignedConfigAckResponse, error) {
	device, _, err := r.userDevice(ctx, userID, request.DeviceID)
	if err != nil {
		return SignedConfigAckResponse{}, err
	}
	return r.ack(ctx, request, device, network, now)
}
