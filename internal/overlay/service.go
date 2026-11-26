package overlay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"account/internal/agentproto"
)

type rootAdminContextKey struct{}

// WithRootAdmin marks ctx as an authenticated root-admin control-plane call,
// allowing full management across all XConnect Zero resources.
func WithRootAdmin(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rootAdminContextKey{}, true)
}

func isRootAdmin(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(rootAdminContextKey{}).(bool)
	return v
}

var (
	overlayDeviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	policyRuleIDPattern    = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,64}$`)
)

const maxPolicyRules = 200

type policyCacheKey struct {
	networkID  string
	generation uint64
}

type cachedPolicyEntry struct {
	gatewayPolicy PolicyArtifact
	clientRaw     []byte
	clientDigest  string
}

type Service struct {
	meshNetworks        map[string]bool
	repo                *Repository
	keyID               string
	privateKey          ed25519.PrivateKey
	previousKeyID       string
	previousPublicKey   ed25519.PublicKey
	previousKeyNotAfter time.Time
	enrollmentTTL       time.Duration
	credentialTTL       time.Duration
	signedConfigTTL     time.Duration
	localProxyPort      int
	clock               func() time.Time
	policyCacheMu       sync.RWMutex
	policyCache         map[policyCacheKey]cachedPolicyEntry
}

const adminDeviceACKRecentWindow = 5 * time.Minute

const (
	stableUATEnvironment = "uat"
	stableUATNetworkID   = "net_uat"
	stableUATGatewayID   = "gw-uat-tw-xconnect"
	stableUATGatewayHost = "tw-xconnect.svc.plus"
)

const (
	registrationTTL               = 15 * time.Minute
	registrationPollInterval      = 5
	registrationPendingLimit      = 20
	registrationCreateWindow      = 15 * time.Minute
	registrationCreateWindowLimit = 20
	registrationDailyCreateWindow = 24 * time.Hour
	registrationDailyCreateLimit  = 100
	registrationAdminPageSize     = 100
)

func NewService(db *gorm.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("overlay service requires a database")
	}
	if !cfg.SchemaManaged {
		if err := AutoMigrate(db); err != nil {
			return nil, fmt.Errorf("migrate overlay schema: %w", err)
		}
	}
	cfg = cfg.withDefaults()
	if cfg.LocalProxyPort < 1024 || cfg.LocalProxyPort > 65535 {
		return nil, errors.New("overlay local proxy port must be between 1024 and 65535")
	}
	key := append(ed25519.PrivateKey(nil), cfg.SigningPrivateKey...)
	if len(key) == 0 {
		if cfg.RequireSigningKey {
			return nil, errors.New("overlay signing private key is required (set XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY)")
		}
		var err error
		_, key, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate overlay signing key: %w", err)
		}
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("overlay signing key must be an Ed25519 private key")
	}
	prevPub := append(ed25519.PublicKey(nil), cfg.PreviousSigningPublicKey...)
	prevKeyID := strings.TrimSpace(cfg.PreviousSigningKeyID)
	if len(prevPub) > 0 {
		if len(prevPub) != ed25519.PublicKeySize {
			return nil, errors.New("overlay previous signing key must be an Ed25519 public key")
		}
		if prevKeyID == "" {
			prevKeyID = "zero-signing-previous"
		}
		if prevKeyID == cfg.SigningKeyID {
			return nil, errors.New("overlay previous signing key id must differ from current signing key id")
		}
	}
	repo, err := NewRepository(db)
	if err != nil {
		return nil, err
	}
	meshNetworks := map[string]bool{}
	for _, id := range cfg.MeshNetworks {
		if id = strings.TrimSpace(id); id != "" {
			meshNetworks[id] = true
		}
	}
	return &Service{
		meshNetworks:        meshNetworks,
		repo:                repo,
		keyID:               cfg.SigningKeyID,
		privateKey:          key,
		previousKeyID:       prevKeyID,
		previousPublicKey:   prevPub,
		previousKeyNotAfter: cfg.PreviousSigningKeyNotAfter,
		enrollmentTTL:       cfg.EnrollmentTTL,
		credentialTTL:       cfg.CredentialTTL,
		signedConfigTTL:     cfg.SignedConfigTTL,
		localProxyPort:      cfg.LocalProxyPort,
		clock:               cfg.Clock,
		policyCache:         make(map[policyCacheKey]cachedPolicyEntry),
	}, nil
}

func decodeBase64Flexible(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(raw)
	}
	return decoded, err
}

func parseEd25519PrivateKeyEnv(raw string) (ed25519.PrivateKey, error) {
	decoded, err := decodeBase64Flexible(raw)
	if err != nil {
		return nil, err
	}
	switch len(decoded) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	default:
		return nil, errors.New("invalid Ed25519 private key length")
	}
}

func parseBoolEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ConfigFromEnv is intended for the account service entrypoint. The private
// key is injected by the deployment secret manager (Vault in production),
// encoded as unpadded or padded base64 (either 64-byte Ed25519 private key or
// 32-byte seed). Setting XCONNECT_OVERLAY_REQUIRE_SIGNING_KEY=true enforces
// fail-fast startup when no signing private key is configured.
func ConfigFromEnv() (Config, error) {
	localProxyPort := 0
	if value := strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_LOCAL_PROXY_PORT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1024 || parsed > 65535 {
			return Config{}, errors.New("XCONNECT_OVERLAY_LOCAL_PROXY_PORT must be a valid unprivileged port")
		}
		localProxyPort = parsed
	}
	cfg := Config{
		MeshNetworks:         strings.Split(os.Getenv("XCONNECT_OVERLAY_MESH_NETWORKS"), ","),
		SigningKeyID:         strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_SIGNING_KEY_ID")),
		PreviousSigningKeyID: strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_KEY_ID")),
		RequireSigningKey:    parseBoolEnv(os.Getenv("XCONNECT_OVERLAY_REQUIRE_SIGNING_KEY")),
		LocalProxyPort:       localProxyPort,
	}
	if keyText := strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY")); keyText != "" {
		key, err := parseEd25519PrivateKeyEnv(keyText)
		if err != nil {
			return Config{}, errors.New("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY must be base64 Ed25519 private key")
		}
		cfg.SigningPrivateKey = key
	} else if cfg.RequireSigningKey {
		return Config{}, errors.New("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY is required when XCONNECT_OVERLAY_REQUIRE_SIGNING_KEY is enabled")
	}
	if prevPubText := strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PUBLIC_KEY")); prevPubText != "" {
		pub, err := decodeBase64Flexible(prevPubText)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return Config{}, errors.New("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PUBLIC_KEY must be base64 Ed25519 public key")
		}
		cfg.PreviousSigningPublicKey = ed25519.PublicKey(pub)
	} else if prevPrivText := strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PRIVATE_KEY")); prevPrivText != "" {
		prevPriv, err := parseEd25519PrivateKeyEnv(prevPrivText)
		if err != nil {
			return Config{}, errors.New("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PRIVATE_KEY must be base64 Ed25519 private key")
		}
		cfg.PreviousSigningPublicKey = prevPriv.Public().(ed25519.PublicKey)
	}
	if notAfterText := strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_NOT_AFTER")); notAfterText != "" {
		parsed, err := time.Parse(time.RFC3339, notAfterText)
		if err != nil {
			return Config{}, errors.New("XCONNECT_OVERLAY_PREVIOUS_SIGNING_NOT_AFTER must be RFC3339 timestamp")
		}
		cfg.PreviousSigningKeyNotAfter = canonicalTime(parsed)
	}
	return cfg, nil
}

func (s *Service) Repository() *Repository { return s.repo }

// ReconcileStableGateway repairs only the owner projection for the fixed UAT
// Gateway. The exact identity guard prevents a generic cross-tenant transfer
// or a production backdoor. Users, subscriptions, billing, usage, credentials,
// keys, and ACK rows are deliberately untouched.
func (s *Service) ReconcileStableGateway(ctx context.Context, request StableGatewayReconcileRequest) (StableGatewayReconcileResult, error) {
	if strings.ToLower(strings.TrimSpace(request.Environment)) != stableUATEnvironment ||
		strings.TrimSpace(request.NetworkID) != stableUATNetworkID ||
		strings.TrimSpace(request.GatewayID) != stableUATGatewayID ||
		strings.ToLower(strings.TrimSpace(request.GatewayEndpointHost)) != stableUATGatewayHost ||
		strings.TrimSpace(request.OwnerUserID) == "" {
		return StableGatewayReconcileResult{}, ErrInvalidInput
	}

	result := StableGatewayReconcileResult{
		Environment: stableUATEnvironment, NetworkID: stableUATNetworkID,
		GatewayID: stableUATGatewayID, GatewayEndpointHost: stableUATGatewayHost,
		OwnerUserID: strings.TrimSpace(request.OwnerUserID),
	}
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var network NetworkRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", stableUATNetworkID).First(&network).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if network.GatewayID != stableUATGatewayID {
			return ErrInvalidInput
		}
		// The network ID is the canonical UAT identity. Scope the uniqueness
		// check to that network so a stale record from an older test network
		// cannot block reconciliation of the current stable Gateway.
		var gatewayCount int64
		if err := tx.Model(&NetworkRecord{}).Where("id = ? AND gateway_id = ?", stableUATNetworkID, stableUATGatewayID).Count(&gatewayCount).Error; err != nil {
			return err
		}
		if gatewayCount != 1 {
			return ErrConflict
		}

		result.PreviousOwnerID = strings.TrimSpace(network.OwnerUserID)
		result.OwnerReconciled = result.PreviousOwnerID != result.OwnerUserID
		result.EndpointNormalized = strings.ToLower(strings.TrimSpace(network.GatewayEndpointHost)) != stableUATGatewayHost
		if result.OwnerReconciled || result.EndpointNormalized {
			updates := map[string]any{"updated_at": s.now()}
			if result.OwnerReconciled {
				updates["owner_user_id"] = result.OwnerUserID
			}
			if result.EndpointNormalized {
				updates["gateway_endpoint_host"] = stableUATGatewayHost
			}
			if err := tx.Model(&NetworkRecord{}).Where("id = ?", stableUATNetworkID).Updates(updates).Error; err != nil {
				return err
			}
		}
		if result.OwnerReconciled {
			if err := tx.Model(&DeviceRecord{}).Where("network_id = ?", stableUATNetworkID).Updates(map[string]any{
				"user_uuid": result.OwnerUserID, "user_id": result.OwnerUserID, "updated_at": s.now(),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&RegistrationRecord{}).Where("network_id = ?", stableUATNetworkID).Update("owner_user_id", result.OwnerUserID).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&DeviceRecord{}).Where("network_id = ?", stableUATNetworkID).Count(&result.DeviceCount).Error; err != nil {
			return err
		}
		return tx.Model(&RegistrationRecord{}).Where("network_id = ?", stableUATNetworkID).Count(&result.RegistrationCount).Error
	})
	return result, err
}

func (s *Service) AdminOverview(ctx context.Context, ownerUserID string) (AdminOverview, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return AdminOverview{}, ErrInvalidInput
	}
	var networks int64
	networkQuery := s.repo.DB.WithContext(ctx).Model(&NetworkRecord{})
	if !isRootAdmin(ctx) {
		networkQuery = networkQuery.Where("owner_user_id = ?", ownerUserID)
	}
	if err := networkQuery.Count(&networks).Error; err != nil {
		return AdminOverview{}, err
	}
	adminDevices, err := s.AdminDevices(ctx, ownerUserID)
	if err != nil {
		return AdminOverview{}, err
	}
	var devices, gateways, ones, connectedGateways, connectedOnes int64
	for _, device := range adminDevices {
		if device.Status != "active" {
			continue
		}
		devices++
		connected := device.ConnectionStatus == "recent_ack"
		switch device.Role {
		case RoleGateway:
			gateways++
			if connected {
				connectedGateways++
			}
		case RoleOne:
			ones++
			if connected {
				connectedOnes++
			}
		}
	}
	return AdminOverview{
		Status:       "available",
		NetworkCount: networks,
		DeviceCount:  devices,
		GatewayCount: gateways,
		OneCount:     ones,
		// These legacy overview fields mean recent current-generation ACK,
		// not a live tunnel or WireGuard handshake.
		GatewayStatus: overlayResourceStatus(gateways, connectedGateways, networks),
		OneStatus:     overlayResourceStatus(ones, connectedOnes, networks),
		SigningKeyID:  s.keyID,
	}, nil
}

// The admin read model reports lifecycle state and recent configuration ACKs,
// not a fabricated network handshake. A recent_ack only means that Accounts
// received a current-generation configuration ACK; it does not claim that a
// live tunnel or WireGuard handshake exists.

func overlayResourceStatus(activeCount, connectedCount, networkCount int64) string {
	if connectedCount > 0 {
		return "connected"
	}
	if activeCount > 0 {
		return "active"
	}
	if networkCount > 0 {
		return "pending"
	}
	return "not_configured"
}

func (s *Service) AdminNetworks(ctx context.Context, ownerUserID string) ([]Network, error) {
	var records []NetworkRecord
	query := s.repo.DB.WithContext(ctx).Omit("policy_json")
	if !isRootAdmin(ctx) {
		query = query.Where("owner_user_id = ?", strings.TrimSpace(ownerUserID))
	}
	if err := query.Order("id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]Network, 0, len(records))
	for _, record := range records {
		result = append(result, toNetwork(record))
	}
	return result, nil
}

func (s *Service) AdminDeleteNetwork(ctx context.Context, ownerUserID, networkID string) error {
	return s.repo.DeleteNetwork(ctx, ownerUserID, networkID)
}

func (s *Service) AdminNetworkMembers(ctx context.Context, callerUserID, networkID string) ([]NetworkMemberSummary, error) {
	callerUserID = strings.TrimSpace(callerUserID)
	networkID = strings.TrimSpace(networkID)
	if callerUserID == "" || networkID == "" {
		return nil, ErrInvalidInput
	}
	var network NetworkRecord
	networkQuery := s.repo.DB.WithContext(ctx)
	if isRootAdmin(ctx) {
		networkQuery = networkQuery.Where("id = ?", networkID)
	} else {
		networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", networkID, callerUserID)
	}
	if err := networkQuery.First(&network).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var records []NetworkMemberRecord
	if err := s.repo.DB.WithContext(ctx).
		Where("network_id = ?", network.ID).
		Order("created_at ASC, id ASC").
		Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]NetworkMemberSummary, 0, len(records))
	for _, record := range records {
		result = append(result, toNetworkMemberSummary(record))
	}
	return result, nil
}

func (s *Service) AdminBindNetworkMember(ctx context.Context, callerUserID, networkID, targetUserID, targetEmail string) (NetworkMemberSummary, error) {
	callerUserID = strings.TrimSpace(callerUserID)
	networkID = strings.TrimSpace(networkID)
	targetUserID = strings.TrimSpace(targetUserID)
	targetEmail = strings.TrimSpace(targetEmail)
	if callerUserID == "" || networkID == "" || targetUserID == "" {
		return NetworkMemberSummary{}, ErrInvalidInput
	}
	var bound NetworkMemberRecord
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var network NetworkRecord
		networkQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if isRootAdmin(ctx) {
			networkQuery = networkQuery.Where("id = ?", networkID)
		} else {
			networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", networkID, callerUserID)
		}
		if err := networkQuery.First(&network).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		candidate := NetworkMemberRecord{
			ID:        newID("nmem"),
			NetworkID: network.ID,
			UserID:    targetUserID,
			Email:     targetEmail,
			CreatedBy: callerUserID,
			CreatedAt: canonicalTime(s.now()),
		}
		conflictClause := clause.OnConflict{
			Columns:   []clause.Column{{Name: "network_id"}, {Name: "user_id"}},
			DoNothing: true,
		}
		if targetEmail != "" {
			conflictClause = clause.OnConflict{
				Columns:   []clause.Column{{Name: "network_id"}, {Name: "user_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"email"}),
			}
		}
		if err := tx.Clauses(conflictClause).Create(&candidate).Error; err != nil {
			return err
		}
		return tx.Where("network_id = ? AND user_id = ?", network.ID, targetUserID).First(&bound).Error
	})
	if err != nil {
		return NetworkMemberSummary{}, err
	}
	return toNetworkMemberSummary(bound), nil
}

func (s *Service) AdminRemoveNetworkMember(ctx context.Context, callerUserID, networkID, targetUserID string) error {
	callerUserID = strings.TrimSpace(callerUserID)
	networkID = strings.TrimSpace(networkID)
	targetUserID = strings.TrimSpace(targetUserID)
	if callerUserID == "" || networkID == "" || targetUserID == "" {
		return ErrInvalidInput
	}
	return s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var network NetworkRecord
		networkQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if isRootAdmin(ctx) {
			networkQuery = networkQuery.Where("id = ?", networkID)
		} else {
			networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", networkID, callerUserID)
		}
		if err := networkQuery.First(&network).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		result := tx.Where("network_id = ? AND user_id = ?", network.ID, targetUserID).Delete(&NetworkMemberRecord{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func toNetworkMemberSummary(record NetworkMemberRecord) NetworkMemberSummary {
	return NetworkMemberSummary{
		ID:        record.ID,
		NetworkID: record.NetworkID,
		UserID:    record.UserID,
		Email:     record.Email,
		CreatedBy: record.CreatedBy,
		CreatedAt: record.CreatedAt.UTC(),
	}
}

func (s *Service) AdminDevices(ctx context.Context, ownerUserID string) ([]AdminDevice, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	var networks []NetworkRecord
	networkQuery := s.repo.DB.WithContext(ctx)
	if !isRootAdmin(ctx) {
		networkQuery = networkQuery.Where("owner_user_id = ?", ownerUserID)
	}
	if err := networkQuery.Order("id ASC").Find(&networks).Error; err != nil {
		return nil, err
	}
	if len(networks) == 0 {
		return []AdminDevice{}, nil
	}

	networkByID := make(map[string]NetworkRecord, len(networks))
	networkIDs := make([]string, 0, len(networks))
	for _, network := range networks {
		networkByID[network.ID] = network
		networkIDs = append(networkIDs, network.ID)
	}

	var records []DeviceRecord
	if err := s.repo.DB.WithContext(ctx).Where("network_id IN ?", networkIDs).Order("id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return []AdminDevice{}, nil
	}

	var acks []AckRecord
	ackQuery := s.repo.DB.WithContext(ctx).
		Model(&AckRecord{}).
		Joins("JOIN overlay_networks ON overlay_networks.id = overlay_signed_config_acks.network_id")
	if !isRootAdmin(ctx) {
		ackQuery = ackQuery.Where("overlay_networks.owner_user_id = ?", ownerUserID)
	}
	if err := ackQuery.Find(&acks).Error; err != nil {
		return nil, err
	}
	currentACK := make(map[string]time.Time, len(acks))
	hasAnyACK := make(map[string]bool, len(acks))
	for _, ack := range acks {
		network, ok := networkByID[ack.NetworkID]
		if !ok {
			continue
		}
		key := adminDeviceACKKey(ack.NetworkID, ack.DeviceID)
		hasAnyACK[key] = true
		if ack.Generation != network.ConfigGeneration || ack.ConfigID != configID(network.ID, ack.DeviceID, network.ConfigGeneration) {
			continue
		}
		currentACK[key] = ack.ReceivedAt
	}

	var telemetries []DeviceTelemetryRecord
	if s.repo.DB.Migrator().HasTable(&DeviceTelemetryRecord{}) {
		if err := s.repo.DB.WithContext(ctx).Where("network_id IN ?", networkIDs).Find(&telemetries).Error; err != nil {
			return nil, err
		}
	}
	telemetryByKey := make(map[string]DeviceTelemetryRecord, len(telemetries))
	for _, item := range telemetries {
		telemetryByKey[adminDeviceACKKey(item.NetworkID, item.DeviceID)] = item
	}

	now := s.now()
	result := make([]AdminDevice, 0, len(records))
	for _, record := range records {
		network, ok := networkByID[record.NetworkID]
		if !ok {
			continue
		}
		key := adminDeviceACKKey(network.ID, record.ID)
		ackReceivedAt, acknowledged := currentACK[key]
		var telemetryPtr *DeviceTelemetryRecord
		if item, exists := telemetryByKey[key]; exists {
			telemetryPtr = &item
		}
		result = append(result, toAdminDevice(record, network, ackReceivedAt, acknowledged, hasAnyACK[key], telemetryPtr, now))
	}
	return result, nil
}

// RecordRuntimeObservation marks an existing, active signed-enrollment device
// as recently observed and persists non-secret runtime telemetry. Ownership,
// credentials, WireGuard keys, transport settings, and device status are not
// mutable through this path.
func (s *Service) RecordRuntimeObservation(ctx context.Context, request RuntimeObservationRequest) error {
	networkID := strings.TrimSpace(request.NetworkID)
	deviceID := strings.TrimSpace(request.DeviceID)
	role := strings.TrimSpace(request.Role)
	if networkID == "" && request.Overlay != nil {
		networkID = strings.TrimSpace(request.Overlay.NetworkID)
	}
	if networkID == "" && request.GatewayTelemetry != nil {
		networkID = strings.TrimSpace(request.GatewayTelemetry.NetworkID)
	}
	if deviceID == "" && request.Overlay != nil {
		deviceID = strings.TrimSpace(request.Overlay.DeviceID)
	}
	if deviceID == "" && request.GatewayTelemetry != nil {
		deviceID = strings.TrimSpace(request.GatewayTelemetry.GatewayID)
	}
	if networkID == "" || len(networkID) > 128 || deviceID == "" || len(deviceID) > 128 || (role != RoleGateway && role != RoleOne) {
		return ErrInvalidInput
	}
	if request.Overlay != nil {
		if overlayNet := strings.TrimSpace(request.Overlay.NetworkID); overlayNet != "" && overlayNet != networkID {
			return ErrInvalidInput
		}
		if overlayDev := strings.TrimSpace(request.Overlay.DeviceID); overlayDev != "" && overlayDev != deviceID {
			return ErrInvalidInput
		}
		if len(request.Overlay.Paths) > 256 || len(request.Overlay.Capabilities) > 8 {
			return ErrInvalidInput
		}
	}
	if request.GatewayTelemetry != nil {
		gt := request.GatewayTelemetry
		if gtNet := strings.TrimSpace(gt.NetworkID); gtNet != "" && gtNet != networkID {
			return ErrInvalidInput
		}
		if gtID := strings.TrimSpace(gt.GatewayID); gtID != "" && gtID != deviceID {
			return ErrInvalidInput
		}
		if len(gt.Peers) > 512 || len(gt.TopApps) > 64 {
			return ErrInvalidInput
		}
	}

	now := s.now()
	return s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&DeviceRecord{}).
			Where("id = ? AND network_id = ? AND role = ? AND status = ?", deviceID, networkID, role, "active").
			Updates(map[string]any{"last_seen_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}

		sampledAt := now
		healthy := request.Healthy
		if request.Overlay != nil {
			if !request.Overlay.UpdatedAt.IsZero() {
				sampledAt = canonicalTime(request.Overlay.UpdatedAt)
			}
			if request.Overlay.Healthy {
				healthy = true
			}
		}
		if request.GatewayTelemetry != nil {
			if !request.GatewayTelemetry.SampledAt.IsZero() {
				sampledAt = canonicalTime(request.GatewayTelemetry.SampledAt)
			}
			if request.GatewayTelemetry.WireGuardRunning && request.GatewayTelemetry.XrayRunning {
				healthy = true
			}
		}

		telemetry := DeviceTelemetryRecord{
			NetworkID:   networkID,
			DeviceID:    deviceID,
			Role:        role,
			Healthy:     healthy,
			PeersJSON:   "[]",
			TopAppsJSON: "[]",
			SampledAt:   sampledAt,
			UpdatedAt:   now,
		}
		if request.Overlay != nil && len(request.Overlay.Paths) > 0 {
			firstPath := request.Overlay.Paths[0]
			telemetry.ActivePath = strings.TrimSpace(firstPath.Path)
			telemetry.Endpoint = strings.TrimSpace(firstPath.Endpoint)
			telemetry.RTTMillis = firstPath.RTTMillis
		}
		if request.GatewayTelemetry != nil {
			gt := request.GatewayTelemetry
			telemetry.ConnectionMode = strings.TrimSpace(gt.ConnectionMode)
			telemetry.AppliedConfigID = strings.TrimSpace(gt.AppliedConfigID)
			telemetry.AppliedGeneration = gt.AppliedGeneration
			if telemetry.Endpoint == "" && strings.TrimSpace(gt.EndpointHost) != "" {
				telemetry.Endpoint = strings.TrimSpace(gt.EndpointHost)
			}
			if telemetry.ActivePath == "" {
				if gt.RelayRunning {
					telemetry.ActivePath = "relay"
				} else if gt.WireGuardRunning {
					telemetry.ActivePath = "direct"
				}
			}
			telemetry.RxBytes = gt.Traffic.RxBytesTotal
			telemetry.TxBytes = gt.Traffic.TxBytesTotal
			telemetry.InternetRxBytes = gt.Traffic.InternetRxBytes
			telemetry.InternetTxBytes = gt.Traffic.InternetTxBytes
			telemetry.PrivateRxBytes = gt.Traffic.PrivateRxBytes
			telemetry.PrivateTxBytes = gt.Traffic.PrivateTxBytes
			if len(gt.Peers) > 0 {
				if raw, err := json.Marshal(gt.Peers); err == nil {
					telemetry.PeersJSON = string(raw)
				}
			}
			if len(gt.TopApps) > 0 {
				if raw, err := json.Marshal(gt.TopApps); err == nil {
					telemetry.TopAppsJSON = string(raw)
				}
			}
		}

		upsertCols := []string{
			"role", "healthy", "connection_mode", "applied_config_id", "applied_generation",
			"active_path", "endpoint", "rtt_ms", "rx_bytes", "tx_bytes",
			"internet_rx_bytes", "internet_tx_bytes", "private_rx_bytes", "private_tx_bytes",
			"latest_handshake_at", "peers_json", "top_apps_json", "sampled_at", "updated_at",
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "network_id"}, {Name: "device_id"}},
			DoUpdates: clause.AssignmentColumns(upsertCols),
		}).Create(&telemetry).Error; err != nil {
			return err
		}

		if role == RoleGateway && request.GatewayTelemetry != nil && len(request.GatewayTelemetry.Peers) > 0 {
			gt := request.GatewayTelemetry
			for _, peer := range gt.Peers {
				peerID := strings.TrimSpace(peer.DeviceID)
				if peerID == "" || len(peerID) > 128 || peerID == deviceID {
					continue
				}
				peerResult := tx.Model(&DeviceRecord{}).
					Where("id = ? AND network_id = ? AND role = ? AND status = ?", peerID, networkID, RoleOne, "active").
					Updates(map[string]any{"last_seen_at": now, "updated_at": now})
				if peerResult.Error != nil {
					return peerResult.Error
				}
				if peerResult.RowsAffected != 1 {
					continue
				}
				var handshakeAt *time.Time
				if peer.LatestHandshakeAt != nil && !peer.LatestHandshakeAt.IsZero() {
					ts := canonicalTime(*peer.LatestHandshakeAt)
					handshakeAt = &ts
				}
				peerTelemetry := DeviceTelemetryRecord{
					NetworkID:         networkID,
					DeviceID:          peerID,
					Role:              RoleOne,
					Healthy:           peer.Connected,
					ConnectionMode:    strings.TrimSpace(gt.ConnectionMode),
					AppliedConfigID:   strings.TrimSpace(gt.AppliedConfigID),
					AppliedGeneration: gt.AppliedGeneration,
					ActivePath:        strings.TrimSpace(peer.Path),
					Endpoint:          strings.TrimSpace(peer.Endpoint),
					RTTMillis:         peer.RTTMillis,
					RxBytes:           peer.RxBytes,
					TxBytes:           peer.TxBytes,
					LatestHandshakeAt: handshakeAt,
					PeersJSON:         "[]",
					TopAppsJSON:       "[]",
					SampledAt:         sampledAt,
					UpdatedAt:         now,
				}
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "network_id"}, {Name: "device_id"}},
					DoUpdates: clause.AssignmentColumns(upsertCols),
				}).Create(&peerTelemetry).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Service) AdminTelemetry(ctx context.Context, ownerUserID string) (AdminTelemetry, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return AdminTelemetry{}, ErrInvalidInput
	}
	var networks []NetworkRecord
	networkQuery := s.repo.DB.WithContext(ctx)
	if !isRootAdmin(ctx) {
		networkQuery = networkQuery.Where("owner_user_id = ?", ownerUserID)
	}
	if err := networkQuery.Order("id ASC").Find(&networks).Error; err != nil {
		return AdminTelemetry{}, err
	}
	if len(networks) == 0 {
		return AdminTelemetry{
			Status:         "available",
			TopPrivateApps: []AdminAppTraffic{},
			TopUsers:       []AdminUserTrafficEntry{},
		}, nil
	}
	networkIDs := make([]string, 0, len(networks))
	for _, n := range networks {
		networkIDs = append(networkIDs, n.ID)
	}

	adminDevices, err := s.AdminDevices(ctx, ownerUserID)
	if err != nil {
		return AdminTelemetry{}, err
	}

	var telemetries []DeviceTelemetryRecord
	if s.repo.DB.Migrator().HasTable(&DeviceTelemetryRecord{}) {
		if err := s.repo.DB.WithContext(ctx).Where("network_id IN ?", networkIDs).Find(&telemetries).Error; err != nil {
			return AdminTelemetry{}, err
		}
	}
	telemetryByKey := make(map[string]DeviceTelemetryRecord, len(telemetries))
	var sase AdminSASETraffic
	appAgg := make(map[string]*AdminAppTraffic)
	for _, item := range telemetries {
		telemetryByKey[adminDeviceACKKey(item.NetworkID, item.DeviceID)] = item
		if item.Role == RoleGateway {
			sase.RxBytesTotal += item.RxBytes
			sase.TxBytesTotal += item.TxBytes
			sase.InternetRxBytes += item.InternetRxBytes
			sase.InternetTxBytes += item.InternetTxBytes
			sase.PrivateRxBytes += item.PrivateRxBytes
			sase.PrivateTxBytes += item.PrivateTxBytes
		}
		if strings.TrimSpace(item.TopAppsJSON) != "" && strings.TrimSpace(item.TopAppsJSON) != "[]" {
			var apps []agentproto.GatewayAppTraffic
			if err := json.Unmarshal([]byte(item.TopAppsJSON), &apps); err == nil {
				for _, app := range apps {
					name := strings.TrimSpace(app.App)
					if name == "" {
						continue
					}
					entry, ok := appAgg[name]
					if !ok {
						entry = &AdminAppTraffic{App: name}
						appAgg[name] = entry
					}
					entry.RxBytes += app.RxBytes
					entry.TxBytes += app.TxBytes
					entry.TotalBytes += app.RxBytes + app.TxBytes
					entry.Connections += app.Connections
				}
			}
		}
	}

	var connectedUsers AdminConnectedUsers
	topUsers := make([]AdminUserTrafficEntry, 0)
	var oneRxSum, oneTxSum uint64
	for _, device := range adminDevices {
		if device.Status != "active" {
			continue
		}
		connected := device.ConnectionStatus == "recent_ack" || (device.RuntimeHealthy != nil && *device.RuntimeHealthy)
		switch device.Role {
		case RoleGateway:
			connectedUsers.GatewayTotal++
			if connected {
				connectedUsers.GatewayOnline++
			}
		case RoleOne:
			connectedUsers.ActiveCount++
			if connected {
				connectedUsers.ConnectedCount++
			}
			var rtt int64
			if device.RTTMillis != nil {
				rtt = *device.RTTMillis
			}
			oneRxSum += device.RxBytes
			oneTxSum += device.TxBytes
			topUsers = append(topUsers, AdminUserTrafficEntry{
				DeviceID:         device.ID,
				Name:             device.Name,
				NetworkID:        device.NetworkID,
				WireGuardAddress: device.WireGuardAddress,
				ActivePath:       device.ActivePath,
				Connected:        connected,
				RTTMillis:        rtt,
				RxBytes:          device.RxBytes,
				TxBytes:          device.TxBytes,
				TotalBytes:       device.RxBytes + device.TxBytes,
				LastSeenAt:       device.LastSeenAt,
			})
		}
	}
	if sase.RxBytesTotal == 0 && sase.TxBytesTotal == 0 && (oneRxSum > 0 || oneTxSum > 0) {
		sase.RxBytesTotal = oneRxSum
		sase.TxBytesTotal = oneTxSum
	}

	topPrivateApps := make([]AdminAppTraffic, 0, len(appAgg))
	for _, entry := range appAgg {
		topPrivateApps = append(topPrivateApps, *entry)
	}
	sort.Slice(topPrivateApps, func(i, j int) bool {
		if topPrivateApps[i].TotalBytes == topPrivateApps[j].TotalBytes {
			return topPrivateApps[i].App < topPrivateApps[j].App
		}
		return topPrivateApps[i].TotalBytes > topPrivateApps[j].TotalBytes
	})
	if len(topPrivateApps) > 20 {
		topPrivateApps = topPrivateApps[:20]
	}

	sort.Slice(topUsers, func(i, j int) bool {
		if topUsers[i].TotalBytes == topUsers[j].TotalBytes {
			return topUsers[i].DeviceID < topUsers[j].DeviceID
		}
		return topUsers[i].TotalBytes > topUsers[j].TotalBytes
	})
	if len(topUsers) > 50 {
		topUsers = topUsers[:50]
	}

	return AdminTelemetry{
		Status:         "available",
		SASETraffic:    sase,
		ConnectedUsers: connectedUsers,
		TopPrivateApps: topPrivateApps,
		TopUsers:       topUsers,
	}, nil
}

func (s *Service) AdminUpdateNetworkMode(ctx context.Context, ownerUserID, networkID, mode string) (Network, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	networkID = strings.TrimSpace(networkID)
	mode = strings.TrimSpace(mode)
	if ownerUserID == "" || networkID == "" {
		return Network{}, ErrInvalidInput
	}
	switch mode {
	case ConnectionModeWGVLESSL3, ConnectionModeWGUDPL3, ConnectionModeWGVLESSL2:
	default:
		return Network{}, ErrInvalidInput
	}
	var updated NetworkRecord
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var network NetworkRecord
		networkQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if isRootAdmin(ctx) {
			networkQuery = networkQuery.Where("id = ?", networkID)
		} else {
			networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", networkID, ownerUserID)
		}
		if err := networkQuery.First(&network).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		now := s.now()
		if err := tx.Model(&NetworkRecord{}).Where("id = ?", network.ID).Updates(map[string]any{
			"connection_mode": mode,
			"updated_at":      now,
		}).Error; err != nil {
			return err
		}
		network.ConnectionMode = mode
		network.UpdatedAt = now
		if err := advanceNetworkGeneration(tx, &network); err != nil {
			return err
		}
		updated = network
		return nil
	})
	if err != nil {
		return Network{}, err
	}
	return toNetwork(updated), nil
}

func (s *Service) UserGateways(ctx context.Context, userID string) ([]UserGatewaySummary, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, ErrInvalidInput
	}
	var networks []NetworkRecord
	userDevicesSubquery := s.repo.DB.WithContext(ctx).Model(&DeviceRecord{}).Select("network_id").Where("user_id = ? AND status = ?", userID, "active")
	networkMembersSubquery := s.repo.DB.WithContext(ctx).Model(&NetworkMemberRecord{}).Select("network_id").Where("user_id = ?", userID)
	if err := s.repo.DB.WithContext(ctx).
		Omit("policy_json").
		Where("owner_user_id = ? OR id IN (?) OR id IN (?)", userID, userDevicesSubquery, networkMembersSubquery).
		Order("id ASC").
		Find(&networks).Error; err != nil {
		return nil, err
	}
	if len(networks) == 0 {
		return []UserGatewaySummary{}, nil
	}
	networkIDs := make([]string, 0, len(networks))
	for _, network := range networks {
		networkIDs = append(networkIDs, network.ID)
	}

	var gwDevices []DeviceRecord
	if err := s.repo.DB.WithContext(ctx).
		Where("network_id IN ? AND role = ? AND status = ?", networkIDs, RoleGateway, "active").
		Find(&gwDevices).Error; err != nil {
		return nil, err
	}
	gwDeviceByNetwork := make(map[string]DeviceRecord, len(gwDevices))
	for _, dev := range gwDevices {
		gwDeviceByNetwork[dev.NetworkID] = dev
	}

	var gwTelemetries []DeviceTelemetryRecord
	if s.repo.DB.Migrator().HasTable(&DeviceTelemetryRecord{}) {
		if err := s.repo.DB.WithContext(ctx).
			Where("network_id IN ? AND role = ?", networkIDs, RoleGateway).
			Find(&gwTelemetries).Error; err != nil {
			return nil, err
		}
	}
	gwTelemetryByNetwork := make(map[string]DeviceTelemetryRecord, len(gwTelemetries))
	for _, item := range gwTelemetries {
		gwTelemetryByNetwork[item.NetworkID] = item
	}

	now := s.now()
	result := make([]UserGatewaySummary, 0, len(networks))
	for _, network := range networks {
		mode := strings.TrimSpace(network.ConnectionMode)
		if mode == "" {
			mode = DefaultConnectionMode
		}
		var lastSeenAt *time.Time
		runtimeHealthy := false
		if gwDev, ok := gwDeviceByNetwork[network.ID]; ok && gwDev.LastSeenAt != nil {
			lastSeenAt = gwDev.LastSeenAt
			if !gwDev.LastSeenAt.After(now) && !gwDev.LastSeenAt.Before(now.Add(-adminDeviceACKRecentWindow)) {
				runtimeHealthy = true
			}
		}
		if gwTel, ok := gwTelemetryByNetwork[network.ID]; ok {
			runtimeHealthy = gwTel.Healthy
			if lastSeenAt == nil {
				ts := gwTel.UpdatedAt
				lastSeenAt = &ts
			}
		}
		result = append(result, UserGatewaySummary{
			NetworkID:           network.ID,
			DisplayName:         network.DisplayName,
			CIDR:                network.CIDR,
			GatewayID:           network.GatewayID,
			GatewayEndpointHost: network.GatewayEndpointHost,
			GatewayEndpointPort: network.GatewayEndpointPort,
			TransportServerName: network.TransportServerName,
			TransportPort:       network.TransportPort,
			ConnectionMode:      mode,
			RuntimeHealthy:      runtimeHealthy,
			LastSeenAt:          lastSeenAt,
		})
	}
	return result, nil
}

func (s *Service) UserEnrollDevice(ctx context.Context, userID string, request UserEnrollRequest) (ExchangeResponse, error) {
	userID = strings.TrimSpace(userID)
	request.NetworkID = strings.TrimSpace(request.NetworkID)
	request.GatewayID = strings.TrimSpace(request.GatewayID)
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	request.Name = strings.TrimSpace(request.Name)
	request.Platform = strings.TrimSpace(request.Platform)
	request.Hostname = strings.TrimSpace(request.Hostname)
	request.WireGuardPublicKey = strings.TrimSpace(request.WireGuardPublicKey)

	publicKey, keyErr := base64.StdEncoding.DecodeString(request.WireGuardPublicKey)
	if keyErr != nil {
		publicKey, keyErr = base64.RawStdEncoding.DecodeString(request.WireGuardPublicKey)
	}
	if userID == "" || request.NetworkID == "" || len(request.NetworkID) > 128 || !overlayDeviceIDPattern.MatchString(request.DeviceID) || keyErr != nil || len(publicKey) != 32 || len(request.Name) > 255 || len(request.Hostname) > 255 {
		return ExchangeResponse{}, ErrInvalidInput
	}
	switch request.Platform {
	case "linux", "darwin", "windows", "ios", "android":
	default:
		return ExchangeResponse{}, ErrInvalidInput
	}
	request.WireGuardPublicKey = base64.StdEncoding.EncodeToString(publicKey)
	if request.Name == "" {
		request.Name = request.DeviceID
	}
	if request.Hostname == "" {
		request.Hostname = request.DeviceID
	}

	now := s.now()
	credentialRawID := randomHex(16)
	credentialID := "xdcid_" + credentialRawID
	credentialSecret := "xdc_" + credentialRawID + "." + newOpaqueToken("")
	enrollmentSecret := newOpaqueToken("xenr_")
	credentialIssued := canonicalTime(now)
	credentialExpiry := canonicalTime(now.Add(s.credentialTTL))
	enrollmentExpiry := canonicalTime(now.Add(s.enrollmentTTL))

	var device DeviceRecord
	var network NetworkRecord
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		networkRecord, err := s.repo.networkTx(tx, request.NetworkID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(networkRecord.OwnerUserID) != userID {
			var memberCount int64
			if err := tx.Model(&NetworkMemberRecord{}).
				Where("network_id = ? AND user_id = ?", networkRecord.ID, userID).
				Count(&memberCount).Error; err != nil {
				return err
			}
			if memberCount == 0 {
				return ErrForbidden
			}
		}
		if request.GatewayID != "" && request.GatewayID != networkRecord.GatewayID {
			return ErrInvalidInput
		}
		if request.DeviceID == networkRecord.GatewayID || request.WireGuardPublicKey == networkRecord.GatewayWireGuardKey {
			return ErrDeviceConflict
		}

		var existing DeviceRecord
		existingErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", request.DeviceID).First(&existing).Error
		if existingErr == nil {
			sameOwner := existing.UserID == userID ||
				(strings.TrimSpace(existing.UserID) == "" && strings.TrimSpace(existing.UserUUID) == userID) ||
				strings.TrimSpace(networkRecord.OwnerUserID) == userID ||
				isRootAdmin(ctx)
			if existing.NetworkID != networkRecord.ID || existing.Role != RoleOne || !sameOwner {
				return ErrDeviceConflict
			}
			var keyConflict int64
			if err := tx.Model(&DeviceRecord{}).
				Where("network_id = ? AND id <> ? AND wireguard_public_key = ?", networkRecord.ID, existing.ID, request.WireGuardPublicKey).
				Count(&keyConflict).Error; err != nil {
				return err
			}
			if keyConflict > 0 {
				return ErrDeviceConflict
			}
			address := existing.WireGuardAddress
			if strings.TrimSpace(address) == "" {
				var allocErr error
				address, allocErr = s.repo.allocateAddress(tx, networkRecord)
				if allocErr != nil {
					return allocErr
				}
			}
			if err := tx.Model(&CredentialRecord{}).Where("device_id = ? AND revoked_at IS NULL", existing.ID).Update("revoked_at", now).Error; err != nil {
				return err
			}
			if err := tx.Model(&DeviceRecord{}).Where("id = ? AND network_id = ?", existing.ID, networkRecord.ID).Updates(map[string]any{
				"name":                 request.Name,
				"platform":             request.Platform,
				"hostname":             request.Hostname,
				"wireguard_public_key": request.WireGuardPublicKey,
				"wireguard_address":    address,
				"status":               "active",
				"updated_at":           now,
			}).Error; err != nil {
				return err
			}
			existing.Name = request.Name
			existing.Platform = request.Platform
			existing.Hostname = request.Hostname
			existing.WireGuardPublicKey = request.WireGuardPublicKey
			existing.WireGuardAddress = address
			existing.Status = "active"
			existing.UpdatedAt = now
			device = existing
		} else if errors.Is(existingErr, gorm.ErrRecordNotFound) {
			var keyConflict int64
			if err := tx.Model(&DeviceRecord{}).
				Where("network_id = ? AND wireguard_public_key = ?", networkRecord.ID, request.WireGuardPublicKey).
				Count(&keyConflict).Error; err != nil {
				return err
			}
			if keyConflict > 0 {
				return ErrDeviceConflict
			}
			address, err := s.repo.allocateAddress(tx, networkRecord)
			if err != nil {
				return err
			}
			device = DeviceRecord{
				ID:                 request.DeviceID,
				UserUUID:           userID,
				UserID:             userID,
				NetworkID:          networkRecord.ID,
				Role:               RoleOne,
				Name:               request.Name,
				Platform:           request.Platform,
				Hostname:           request.Hostname,
				WireGuardPublicKey: request.WireGuardPublicKey,
				WireGuardAddress:   address,
				Status:             "active",
				CreatedAt:          now,
				UpdatedAt:          now,
			}
			if err := tx.Create(&device).Error; err != nil {
				return err
			}
		} else {
			return existingErr
		}

		if err := advanceNetworkGeneration(tx, &networkRecord); err != nil {
			return err
		}
		credential := CredentialRecord{ID: newID("cred"), DeviceID: device.ID, CredentialID: credentialID, TokenHash: HashSecret(credentialSecret), IssuedAt: credentialIssued, ExpiresAt: credentialExpiry}
		enrollment := EnrollmentRecord{ID: newID("enr"), DeviceID: device.ID, TokenHash: HashSecret(enrollmentSecret), IssuedAt: credentialIssued, ExpiresAt: enrollmentExpiry}
		if err := tx.Create(&credential).Error; err != nil {
			return err
		}
		if err := tx.Create(&enrollment).Error; err != nil {
			return err
		}
		network = networkRecord
		return nil
	})
	if err != nil {
		return ExchangeResponse{}, err
	}
	return ExchangeResponse{
		EnrollmentToken: enrollmentSecret,
		TokenType:       TokenTypeBearer,
		ExpiresAt:       enrollmentExpiry,
		Scope:           []string{ScopeConfigRead, ScopeConfigAck, ScopeDeviceRevoke},
		DeviceCredential: DeviceCredential{
			CredentialID: credentialID,
			Credential:   credentialSecret,
			TokenType:    TokenTypeDevice,
			IssuedAt:     credentialIssued,
			ExpiresAt:    credentialExpiry,
			Scope:        []string{ScopeSessionMint, ScopeRotate, ScopeDeviceRevoke},
		},
		Device:      toDevice(device),
		Network:     toNetwork(network),
		SigningKeys: s.SigningKeys(now),
	}, nil
}

func adminDeviceACKKey(networkID, deviceID string) string { return networkID + "\x00" + deviceID }

func (s *Service) AdminInvites(ctx context.Context, ownerUserID string) ([]InviteSummary, error) {
	var records []InviteRecord
	query := s.repo.DB.WithContext(ctx)
	if !isRootAdmin(ctx) {
		ownedNetworks := s.repo.DB.WithContext(ctx).Model(&NetworkRecord{}).Select("id").Where("owner_user_id = ?", strings.TrimSpace(ownerUserID))
		query = query.Where("network_id IN (?)", ownedNetworks)
	}
	if err := query.Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]InviteSummary, 0, len(records))
	for _, record := range records {
		result = append(result, InviteSummary{ID: record.ID, NetworkID: record.NetworkID, DeviceID: record.DeviceID, Platform: record.Platform, Role: record.Role, ExpiresAt: record.ExpiresAt.UTC(), RemainingUses: record.RemainingUses, ConsumedAt: record.ConsumedAt, CreatedAt: record.CreatedAt.UTC()})
	}
	return result, nil
}

// AdminCreateInvite creates a single device-bound enrollment invitation for an
// existing network owned by the authenticated account. The raw invitation is
// intentionally returned only in this response; persistence keeps its digest.
func (s *Service) AdminCreateInvite(ctx context.Context, ownerUserID, controllerURL string, request AdminInviteRequest) (AdminInviteResult, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	controllerURL = strings.TrimRight(strings.TrimSpace(controllerURL), "/")
	request.NetworkID = strings.TrimSpace(request.NetworkID)
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	request.Platform = strings.TrimSpace(request.Platform)
	request.Role = strings.TrimSpace(request.Role)
	if ownerUserID == "" || controllerURL == "" || request.NetworkID == "" || !overlayDeviceIDPattern.MatchString(request.DeviceID) || request.ExpiresAt.IsZero() || !request.ExpiresAt.After(s.now()) {
		return AdminInviteResult{}, ErrInvalidInput
	}
	if request.Role == "" {
		request.Role = RoleOne
	}
	if request.Role != RoleOne && request.Role != RoleGateway {
		return AdminInviteResult{}, ErrInvalidInput
	}
	switch request.Platform {
	case "linux", "darwin", "windows", "ios", "android":
	default:
		return AdminInviteResult{}, ErrInvalidInput
	}

	var network NetworkRecord
	networkQuery := s.repo.DB.WithContext(ctx)
	if isRootAdmin(ctx) {
		networkQuery = networkQuery.Where("id = ?", request.NetworkID)
	} else {
		networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", request.NetworkID, ownerUserID)
	}
	if err := networkQuery.First(&network).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AdminInviteResult{}, ErrNotFound
		}
		return AdminInviteResult{}, err
	}

	joinToken := newOpaqueToken("xjt_")
	invite := InviteRecord{
		ID:            uuid.NewString(),
		NetworkID:     network.ID,
		TokenHash:     HashSecret(joinToken),
		DeviceID:      request.DeviceID,
		Platform:      request.Platform,
		Role:          request.Role,
		ExpiresAt:     canonicalTime(request.ExpiresAt),
		RemainingUses: 1,
	}
	if err := s.repo.DB.WithContext(ctx).Create(&invite).Error; err != nil {
		return AdminInviteResult{}, err
	}
	return AdminInviteResult{
		Invite:  InviteSummary{ID: invite.ID, NetworkID: invite.NetworkID, DeviceID: invite.DeviceID, Platform: invite.Platform, Role: invite.Role, ExpiresAt: invite.ExpiresAt, RemainingUses: invite.RemainingUses, CreatedAt: invite.CreatedAt.UTC()},
		JoinURI: "xconnect://join/" + joinToken + "?controller=" + url.QueryEscape(controllerURL),
	}, nil
}

func (s *Service) AdminBootstrap(ctx context.Context, cfg BootstrapConfig, controllerURL, joinToken string) (AdminBootstrapResult, error) {
	joinToken, err := s.Seed(ctx, cfg, joinToken)
	if err != nil {
		return AdminBootstrapResult{}, err
	}
	// The UAT Gateway is a fixed machine identity. Its persisted local
	// WireGuard key can legitimately be regenerated during an interrupted UAT
	// recovery, while the device record from the earlier run remains. Reconcile
	// that one exact identity before issuing its fresh, device-bound invite.
	// This is deliberately unavailable to arbitrary networks, Gateways, One
	// devices, or production environments.
	if err := s.reconcileStableUATGatewayKey(ctx, cfg); err != nil {
		return AdminBootstrapResult{}, err
	}
	var network NetworkRecord
	if err := s.repo.DB.WithContext(ctx).Where("id = ?", cfg.Network.ID).First(&network).Error; err != nil {
		return AdminBootstrapResult{}, err
	}
	var invite InviteRecord
	if err := s.repo.DB.WithContext(ctx).Where("token_hash = ?", HashSecret(joinToken)).First(&invite).Error; err != nil {
		return AdminBootstrapResult{}, err
	}
	controllerURL = strings.TrimRight(strings.TrimSpace(controllerURL), "/")
	if controllerURL == "" {
		return AdminBootstrapResult{}, ErrInvalidInput
	}
	return AdminBootstrapResult{Network: toNetwork(network), Invite: InviteSummary{ID: invite.ID, NetworkID: invite.NetworkID, DeviceID: invite.DeviceID, Platform: invite.Platform, Role: invite.Role, ExpiresAt: invite.ExpiresAt.UTC(), RemainingUses: invite.RemainingUses, CreatedAt: invite.CreatedAt.UTC()}, JoinURI: "xconnect://join/" + joinToken + "?controller=" + url.QueryEscape(controllerURL)}, nil
}

func (s *Service) reconcileStableUATGatewayKey(ctx context.Context, cfg BootstrapConfig) error {
	if cfg.Network.ID != stableUATNetworkID || cfg.Network.GatewayID != stableUATGatewayID ||
		strings.ToLower(strings.TrimSpace(cfg.Network.GatewayEndpointHost)) != stableUATGatewayHost ||
		cfg.Invite.DeviceID != stableUATGatewayID || cfg.Invite.Role != RoleGateway || cfg.Invite.Platform != "linux" {
		return nil
	}

	return s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var network NetworkRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", stableUATNetworkID).First(&network).Error; err != nil {
			return err
		}
		var device DeviceRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND network_id = ?", stableUATGatewayID, stableUATNetworkID).First(&device).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if device.Role != RoleGateway {
			return ErrConflict
		}
		if device.WireGuardPublicKey == network.GatewayWireGuardKey && device.Status == "active" {
			return nil
		}

		now := s.now()
		if err := tx.Model(&CredentialRecord{}).Where("device_id = ? AND revoked_at IS NULL", device.ID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&DeviceRecord{}).Where("id = ? AND network_id = ?", device.ID, network.ID).Updates(map[string]any{
			"wireguard_public_key": network.GatewayWireGuardKey,
			"wireguard_address":    network.GatewayWireGuardAddress,
			"status":               "active",
			"updated_at":           now,
		}).Error; err != nil {
			return err
		}
		return advanceNetworkGeneration(tx, &network)
	})
}

func (s *Service) AdminRevokeDevice(ctx context.Context, ownerUserID, deviceID string) error {
	ownerUserID = strings.TrimSpace(ownerUserID)
	deviceID = strings.TrimSpace(deviceID)
	if ownerUserID == "" || deviceID == "" {
		return ErrInvalidInput
	}
	now := s.now()
	return s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&DeviceRecord{}).Where("id = ? AND status = ?", deviceID, "active")
		if !isRootAdmin(ctx) {
			ownedNetworks := tx.Model(&NetworkRecord{}).Select("id").Where("owner_user_id = ?", ownerUserID)
			query = query.Where("network_id IN (?)", ownedNetworks)
		}
		result := query.Updates(map[string]any{"status": "revoked", "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Model(&CredentialRecord{}).Where("device_id = ? AND revoked_at IS NULL", deviceID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		var revoked DeviceRecord
		if err := tx.Where("id = ?", deviceID).First(&revoked).Error; err != nil {
			return err
		}
		if s.meshNetworks[revoked.NetworkID] {
			network, err := s.repo.networkTx(tx, revoked.NetworkID)
			if err != nil {
				return err
			}
			return advanceNetworkGeneration(tx, &network)
		}
		return nil
	})
}

func (s *Service) AdminPolicy(ctx context.Context, ownerUserID, networkID string) (PolicyArtifact, error) {
	var network NetworkRecord
	query := s.repo.DB.WithContext(ctx)
	if isRootAdmin(ctx) {
		query = query.Where("id = ?", strings.TrimSpace(networkID))
	} else {
		query = query.Where("id = ? AND owner_user_id = ?", strings.TrimSpace(networkID), strings.TrimSpace(ownerUserID))
	}
	if err := query.First(&network).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PolicyArtifact{}, ErrNotFound
		}
		return PolicyArtifact{}, err
	}
	return s.policyArtifact(network)
}

func (s *Service) AdminUpdatePolicy(ctx context.Context, ownerUserID, networkID string, policy PolicyArtifact) (PolicyArtifact, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	networkID = strings.TrimSpace(networkID)
	if ownerUserID == "" || networkID == "" {
		return PolicyArtifact{}, ErrNotFound
	}
	var result PolicyArtifact
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var network NetworkRecord
		networkQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if isRootAdmin(ctx) {
			networkQuery = networkQuery.Where("id = ?", networkID)
		} else {
			networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", networkID, ownerUserID)
		}
		if err := networkQuery.First(&network).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		generation := network.ConfigGeneration + 1
		normalized, err := validateAndNormalizePolicy(network.ID, generation, policy)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(normalized)
		if err != nil {
			return err
		}
		updateRes := tx.Model(&NetworkRecord{}).Where("id = ? AND config_generation = ?", network.ID, network.ConfigGeneration).Updates(map[string]any{"policy_json": string(raw), "config_generation": generation})
		if updateRes.Error != nil {
			return updateRes.Error
		}
		if updateRes.RowsAffected != 1 {
			return ErrGenerationConflict
		}
		result = normalized
		return nil
	})
	if err == nil {
		s.evictPolicyCache(networkID)
	}
	return result, err
}

func (s *Service) Seed(ctx context.Context, cfg BootstrapConfig, joinToken string) (string, error) {
	return s.repo.Seed(ctx, cfg, joinToken)
}

func (s *Service) SigningKeys(now time.Time) []SigningKey {
	now = canonicalTime(now)
	currentPub := s.privateKey.Public().(ed25519.PublicKey)
	keys := []SigningKey{
		{
			KeyID:     s.keyID,
			Algorithm: "Ed25519",
			PublicKey: base64.StdEncoding.EncodeToString(currentPub),
			Status:    "current",
			NotBefore: now.Add(-time.Minute),
		},
	}
	if len(s.previousPublicKey) == ed25519.PublicKeySize && !bytes.Equal(s.previousPublicKey, currentPub) {
		notAfter := s.previousKeyNotAfter
		if notAfter.IsZero() {
			notAfter = now.Add(24 * time.Hour)
		}
		notAfter = canonicalTime(notAfter)
		if notAfter.After(now) {
			keys = append(keys, SigningKey{
				KeyID:     s.previousKeyID,
				Algorithm: "Ed25519",
				PublicKey: base64.StdEncoding.EncodeToString(s.previousPublicKey),
				Status:    "previous",
				NotBefore: now.Add(-time.Minute),
				NotAfter:  &notAfter,
			})
		}
	}
	return keys
}

// Register records a request for an owner to approve a One identity. This is
// deliberately not an enrollment path: no device, route, credential, peer, or
// signed configuration is created until a later approved exchange succeeds.
func (s *Service) Register(ctx context.Context, request RegistrationRequest) (RegistrationResponse, error) {
	request.NetworkID = strings.TrimSpace(request.NetworkID)
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	request.Name = strings.TrimSpace(request.Name)
	request.Hostname = strings.TrimSpace(request.Hostname)
	request.Platform = strings.TrimSpace(request.Platform)
	request.WireGuardPublicKey = strings.TrimSpace(request.WireGuardPublicKey)
	publicKey, keyErr := base64.StdEncoding.DecodeString(request.WireGuardPublicKey)
	if keyErr != nil {
		publicKey, keyErr = base64.RawStdEncoding.DecodeString(request.WireGuardPublicKey)
	}
	if request.NetworkID == "" || len(request.NetworkID) > 128 || !overlayDeviceIDPattern.MatchString(request.DeviceID) || keyErr != nil || len(publicKey) != 32 || len(request.Name) > 255 || len(request.Hostname) > 255 {
		return RegistrationResponse{}, ErrInvalidInput
	}
	switch request.Platform {
	case "linux", "darwin", "windows":
	default:
		return RegistrationResponse{}, ErrInvalidInput
	}
	// Persist one canonical representation. The fingerprint is derived from
	// the decoded 32-byte key, never from caller-controlled base64 spelling.
	request.WireGuardPublicKey = base64.StdEncoding.EncodeToString(publicKey)
	publicKeyFingerprint := fingerprintWireGuardPublicKey(publicKey)

	now := s.now()
	token := newOpaqueToken("xrt_")
	record := RegistrationRecord{
		ID:                            newID("xreg"),
		NetworkID:                     request.NetworkID,
		DeviceID:                      request.DeviceID,
		Name:                          request.Name,
		Hostname:                      request.Hostname,
		Platform:                      request.Platform,
		WireGuardPublicKey:            request.WireGuardPublicKey,
		WireGuardPublicKeyFingerprint: publicKeyFingerprint,
		TokenHash:                     HashSecret(token),
		Status:                        RegistrationStatusPending,
		ExpiresAt:                     canonicalTime(now.Add(registrationTTL)),
		CreatedAt:                     canonicalTime(now),
		UpdatedAt:                     canonicalTime(now),
	}
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		network, err := s.repo.networkTx(tx, record.NetworkID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrRegistrationNotAvailable
			}
			return err
		}
		if strings.TrimSpace(network.OwnerUserID) == "" {
			return ErrRegistrationNotAvailable
		}
		record.OwnerUserID = network.OwnerUserID

		var existingDevices int64
		if err := tx.Model(&DeviceRecord{}).Where("network_id = ? AND (id = ? OR wireguard_public_key = ?)", record.NetworkID, record.DeviceID, record.WireGuardPublicKey).Count(&existingDevices).Error; err != nil {
			return err
		}
		if existingDevices != 0 {
			return ErrDeviceConflict
		}
		var sameIdentity int64
		if err := tx.Model(&RegistrationRecord{}).Where("network_id = ? AND device_id = ? AND wireguard_public_key_fingerprint = ? AND status = ? AND expires_at > ?", record.NetworkID, record.DeviceID, record.WireGuardPublicKeyFingerprint, RegistrationStatusPending, now).Count(&sameIdentity).Error; err != nil {
			return err
		}
		if sameIdentity != 0 {
			return ErrRegistrationLimited
		}
		var pending int64
		if err := tx.Model(&RegistrationRecord{}).Where("network_id = ? AND status = ? AND expires_at > ?", record.NetworkID, RegistrationStatusPending, now).Count(&pending).Error; err != nil {
			return err
		}
		if pending >= registrationPendingLimit {
			return ErrRegistrationLimited
		}
		var recentCreates int64
		if err := tx.Model(&RegistrationRecord{}).Where("network_id = ? AND created_at >= ?", record.NetworkID, now.Add(-registrationCreateWindow)).Count(&recentCreates).Error; err != nil {
			return err
		}
		if recentCreates >= registrationCreateWindowLimit {
			return ErrRegistrationLimited
		}
		var dailyCreates int64
		if err := tx.Model(&RegistrationRecord{}).Where("network_id = ? AND created_at >= ?", record.NetworkID, now.Add(-registrationDailyCreateWindow)).Count(&dailyCreates).Error; err != nil {
			return err
		}
		if dailyCreates >= registrationDailyCreateLimit {
			return ErrRegistrationLimited
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return RegistrationResponse{}, err
	}
	return RegistrationResponse{RegistrationID: record.ID, RegistrationToken: token, Status: record.Status, ExpiresAt: record.ExpiresAt, Interval: registrationPollInterval}, nil
}

// ExchangeRegistration uses only the approved stored declaration. It never
// accepts caller-supplied platform, role, public key, owner, or network fields.
func (s *Service) ExchangeRegistration(ctx context.Context, registrationID, token string) (ExchangeResponse, RegistrationPendingResponse, error) {
	registrationID = strings.TrimSpace(registrationID)
	token = strings.TrimSpace(token)
	if registrationID == "" || !validOpaque(token, "xrt_") {
		return ExchangeResponse{}, RegistrationPendingResponse{}, ErrInvalidRegistrationToken
	}
	now := s.now()
	credentialRawID := randomHex(16)
	credentialID := "xdcid_" + credentialRawID
	credentialSecret := "xdc_" + credentialRawID + "." + newOpaqueToken("")
	enrollmentSecret := newOpaqueToken("xenr_")
	credentialIssued := canonicalTime(now)
	credentialExpiry := canonicalTime(now.Add(s.credentialTTL))
	enrollmentExpiry := canonicalTime(now.Add(s.enrollmentTTL))
	var device DeviceRecord
	var network NetworkRecord
	pending := RegistrationPendingResponse{}
	terminalErr := error(nil)
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var registration RegistrationRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND token_hash = ?", registrationID, HashSecret(token)).First(&registration).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInvalidRegistrationToken
			}
			return err
		}
		switch registration.Status {
		case RegistrationStatusRejected:
			return ErrRegistrationRejected
		case RegistrationStatusConsumed:
			return ErrRegistrationConsumed
		case RegistrationStatusExpired:
			return ErrRegistrationExpired
		case RegistrationStatusPending, RegistrationStatusApproved:
		default:
			return ErrRegistrationNotPending
		}
		if !registration.ExpiresAt.After(now) {
			if err := tx.Model(&RegistrationRecord{}).Where("id = ? AND status IN ?", registration.ID, []string{RegistrationStatusPending, RegistrationStatusApproved}).Updates(map[string]any{"status": RegistrationStatusExpired, "updated_at": now}).Error; err != nil {
				return err
			}
			terminalErr = ErrRegistrationExpired
			return nil
		}
		if registration.Status == RegistrationStatusPending {
			pending = RegistrationPendingResponse{Status: RegistrationStatusPending, ExpiresAt: registration.ExpiresAt.UTC(), Interval: registrationPollInterval}
			return ErrRegistrationPending
		}
		networkRecord, err := s.repo.networkTx(tx, registration.NetworkID)
		if err != nil {
			return err
		}
		if networkRecord.OwnerUserID != registration.OwnerUserID {
			return ErrRegistrationNotAvailable
		}
		var existing int64
		if err := tx.Model(&DeviceRecord{}).Where("network_id = ? AND (id = ? OR wireguard_public_key = ?)", registration.NetworkID, registration.DeviceID, registration.WireGuardPublicKey).Count(&existing).Error; err != nil {
			return err
		}
		if existing != 0 {
			return ErrDeviceConflict
		}
		address, err := s.repo.allocateAddress(tx, networkRecord)
		if err != nil {
			return err
		}
		device = DeviceRecord{ID: registration.DeviceID, UserUUID: networkRecord.OwnerUserID, UserID: networkRecord.OwnerUserID, NetworkID: networkRecord.ID, Role: RoleOne, Name: registration.Name, Platform: registration.Platform, Hostname: registration.Hostname, WireGuardPublicKey: registration.WireGuardPublicKey, WireGuardAddress: address, Status: "active", CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&device).Error; err != nil {
			return err
		}
		consumedAt := now
		result := tx.Model(&RegistrationRecord{}).Where("id = ? AND status = ? AND consumed_at IS NULL", registration.ID, RegistrationStatusApproved).Updates(map[string]any{"status": RegistrationStatusConsumed, "consumed_at": consumedAt, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRegistrationConsumed
		}
		if err := advanceNetworkGeneration(tx, &networkRecord); err != nil {
			return err
		}
		credential := CredentialRecord{ID: newID("cred"), DeviceID: device.ID, CredentialID: credentialID, TokenHash: HashSecret(credentialSecret), IssuedAt: credentialIssued, ExpiresAt: credentialExpiry}
		enrollment := EnrollmentRecord{ID: newID("enr"), DeviceID: device.ID, TokenHash: HashSecret(enrollmentSecret), IssuedAt: credentialIssued, ExpiresAt: enrollmentExpiry}
		if err := tx.Create(&credential).Error; err != nil {
			return err
		}
		if err := tx.Create(&enrollment).Error; err != nil {
			return err
		}
		network = networkRecord
		return nil
	})
	if terminalErr != nil {
		return ExchangeResponse{}, RegistrationPendingResponse{}, terminalErr
	}
	if err != nil {
		return ExchangeResponse{}, pending, err
	}
	return ExchangeResponse{EnrollmentToken: enrollmentSecret, TokenType: TokenTypeBearer, ExpiresAt: enrollmentExpiry, Scope: []string{ScopeConfigRead, ScopeConfigAck, ScopeDeviceRevoke}, DeviceCredential: DeviceCredential{CredentialID: credentialID, Credential: credentialSecret, TokenType: TokenTypeDevice, IssuedAt: credentialIssued, ExpiresAt: credentialExpiry, Scope: []string{ScopeSessionMint, ScopeRotate, ScopeDeviceRevoke}}, Device: toDevice(device), Network: toNetwork(network), SigningKeys: s.SigningKeys(now)}, RegistrationPendingResponse{}, nil
}

func (s *Service) AdminRegistrations(ctx context.Context, ownerUserID, cursor string) (RegistrationPage, error) {
	ownerUserID, cursor = strings.TrimSpace(ownerUserID), strings.TrimSpace(cursor)
	if ownerUserID == "" {
		return RegistrationPage{}, ErrInvalidInput
	}
	now := s.now()
	// An approved request still has an absolute exchange deadline. Mark both
	// actionable states expired for a stable owner-facing read model.
	expireQuery := s.repo.DB.WithContext(ctx).Model(&RegistrationRecord{}).
		Where("status IN ? AND expires_at <= ?", []string{RegistrationStatusPending, RegistrationStatusApproved}, now)
	if !isRootAdmin(ctx) {
		expireQuery = expireQuery.Where("owner_user_id = ?", ownerUserID)
	}
	if err := expireQuery.Updates(map[string]any{"status": RegistrationStatusExpired, "updated_at": now}).Error; err != nil {
		return RegistrationPage{}, err
	}
	query := s.repo.DB.WithContext(ctx).Model(&RegistrationRecord{}).
		Select("overlay_registrations.*").
		Joins("JOIN overlay_networks ON overlay_networks.id = overlay_registrations.network_id")
	if !isRootAdmin(ctx) {
		query = query.Where("overlay_registrations.owner_user_id = ? AND overlay_networks.owner_user_id = ?", ownerUserID, ownerUserID)
	}
	if cursor != "" {
		var anchor RegistrationRecord
		anchorQuery := s.repo.DB.WithContext(ctx).Model(&RegistrationRecord{}).
			Joins("JOIN overlay_networks ON overlay_networks.id = overlay_registrations.network_id")
		if isRootAdmin(ctx) {
			anchorQuery = anchorQuery.Where("overlay_registrations.id = ?", cursor)
		} else {
			anchorQuery = anchorQuery.Where("overlay_registrations.id = ? AND overlay_registrations.owner_user_id = ? AND overlay_networks.owner_user_id = ?", cursor, ownerUserID, ownerUserID)
		}
		if err := anchorQuery.First(&anchor).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return RegistrationPage{}, ErrNotFound
			}
			return RegistrationPage{}, err
		}
		query = query.Where("(overlay_registrations.created_at < ?) OR (overlay_registrations.created_at = ? AND overlay_registrations.id < ?)", anchor.CreatedAt, anchor.CreatedAt, anchor.ID)
	}
	var records []RegistrationRecord
	if err := query.Order("overlay_registrations.created_at DESC").Order("overlay_registrations.id DESC").Limit(registrationAdminPageSize + 1).Find(&records).Error; err != nil {
		return RegistrationPage{}, err
	}
	page := RegistrationPage{Registrations: make([]RegistrationSummary, 0, min(len(records), registrationAdminPageSize))}
	if len(records) > registrationAdminPageSize {
		page.HasMore = true
		records = records[:registrationAdminPageSize]
		page.NextCursor = records[len(records)-1].ID
	}
	for _, record := range records {
		page.Registrations = append(page.Registrations, toRegistrationSummary(record))
	}
	return page, nil
}

func (s *Service) AdminApproveRegistration(ctx context.Context, ownerUserID, registrationID, networkID string) (RegistrationSummary, error) {
	return s.adminTransitionRegistration(ctx, ownerUserID, registrationID, networkID, RegistrationStatusApproved)
}

func (s *Service) AdminRejectRegistration(ctx context.Context, ownerUserID, registrationID string) (RegistrationSummary, error) {
	return s.adminTransitionRegistration(ctx, ownerUserID, registrationID, "", RegistrationStatusRejected)
}

func (s *Service) adminTransitionRegistration(ctx context.Context, ownerUserID, registrationID, requestedNetworkID, nextStatus string) (RegistrationSummary, error) {
	ownerUserID, registrationID, requestedNetworkID = strings.TrimSpace(ownerUserID), strings.TrimSpace(registrationID), strings.TrimSpace(requestedNetworkID)
	if ownerUserID == "" || registrationID == "" || nextStatus != RegistrationStatusApproved && nextStatus != RegistrationStatusRejected || nextStatus == RegistrationStatusApproved && requestedNetworkID == "" {
		return RegistrationSummary{}, ErrInvalidInput
	}
	now := s.now()
	var result RegistrationRecord
	terminalErr := error(nil)
	err := s.repo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var registration RegistrationRecord
		regQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if isRootAdmin(ctx) {
			regQuery = regQuery.Where("id = ?", registrationID)
		} else {
			regQuery = regQuery.Where("id = ? AND owner_user_id = ?", registrationID, ownerUserID)
		}
		if err := regQuery.First(&registration).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if nextStatus == RegistrationStatusApproved && requestedNetworkID != registration.NetworkID {
			return ErrNotFound
		}
		networkQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if isRootAdmin(ctx) {
			networkQuery = networkQuery.Where("id = ?", registration.NetworkID)
		} else {
			networkQuery = networkQuery.Where("id = ? AND owner_user_id = ?", registration.NetworkID, ownerUserID)
		}
		if err := networkQuery.First(&NetworkRecord{}).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if registration.Status != RegistrationStatusPending {
			return ErrRegistrationNotPending
		}
		if !registration.ExpiresAt.After(now) {
			if err := tx.Model(&RegistrationRecord{}).Where("id = ? AND status = ?", registration.ID, RegistrationStatusPending).Updates(map[string]any{"status": RegistrationStatusExpired, "updated_at": now}).Error; err != nil {
				return err
			}
			terminalErr = ErrRegistrationExpired
			return nil
		}
		updates := map[string]any{"status": nextStatus, "updated_at": now}
		if nextStatus == RegistrationStatusApproved {
			updates["approved_at"] = now
		} else {
			updates["rejected_at"] = now
		}
		update := tx.Model(&RegistrationRecord{}).Where("id = ? AND status = ?", registration.ID, RegistrationStatusPending).Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrRegistrationNotPending
		}
		registration.Status = nextStatus
		if nextStatus == RegistrationStatusApproved {
			registration.ApprovedAt = &now
		} else {
			registration.RejectedAt = &now
		}
		result = registration
		return nil
	})
	if terminalErr != nil {
		return RegistrationSummary{}, terminalErr
	}
	if err != nil {
		return RegistrationSummary{}, err
	}
	return toRegistrationSummary(result), nil
}

func toRegistrationSummary(record RegistrationRecord) RegistrationSummary {
	return RegistrationSummary{RegistrationID: record.ID, NetworkID: record.NetworkID, DeviceID: record.DeviceID, Name: record.Name, Hostname: record.Hostname, Platform: record.Platform, Status: record.Status, WireGuardPublicKeyFingerprint: record.WireGuardPublicKeyFingerprint, ExpiresAt: record.ExpiresAt.UTC(), CreatedAt: record.CreatedAt.UTC(), ApprovedAt: record.ApprovedAt, RejectedAt: record.RejectedAt, ConsumedAt: record.ConsumedAt}
}

func fingerprintWireGuardPublicKey(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:])
}

func (s *Service) Exchange(ctx context.Context, request ExchangeRequest) (ExchangeResponse, error) {
	// Registration capabilities are purpose-bound. Reject an xrt token before
	// normal invitation validation so neither pending nor approved requests can
	// ever be consumed through the legacy invite endpoint.
	if validOpaque(strings.TrimSpace(request.JoinToken), "xrt_") {
		return ExchangeResponse{}, ErrInvalidToken
	}
	if request.Role == "" {
		request.Role = RoleOne
	}
	if err := validateExchange(request); err != nil {
		return ExchangeResponse{}, err
	}
	now := s.now()
	tokenHash := HashSecret(request.JoinToken)
	invite, err := s.repo.peekInvite(ctx, tokenHash)
	if err != nil {
		return ExchangeResponse{}, err
	}
	if invite.DeviceID != "" && invite.DeviceID != request.DeviceID || invite.Platform != "" && invite.Platform != request.Platform || invite.Role != "" && invite.Role != request.Role {
		return ExchangeResponse{}, ErrInviteConstraint
	}
	credentialRawID := randomHex(16)
	credentialID := "xdcid_" + credentialRawID
	credentialSecret := "xdc_" + credentialRawID + "." + newOpaqueToken("")
	enrollmentSecret := newOpaqueToken("xenr_")
	credentialIssued := canonicalTime(now)
	credentialExpiry := canonicalTime(now.Add(s.credentialTTL))
	enrollmentExpiry := canonicalTime(now.Add(s.enrollmentTTL))
	var device DeviceRecord
	var network NetworkRecord
	var createErr error
	invite, device, network, createErr = s.repo.createDevice(ctx, tokenHash, request, now, CredentialRecord{ID: newID("cred"), CredentialID: credentialID, TokenHash: HashSecret(credentialSecret), IssuedAt: credentialIssued, ExpiresAt: credentialExpiry}, EnrollmentRecord{ID: newID("enr"), TokenHash: HashSecret(enrollmentSecret), IssuedAt: credentialIssued, ExpiresAt: enrollmentExpiry})
	err = createErr
	if err != nil {
		return ExchangeResponse{}, err
	}
	return ExchangeResponse{EnrollmentToken: enrollmentSecret, TokenType: TokenTypeBearer, ExpiresAt: enrollmentExpiry, Scope: []string{ScopeConfigRead, ScopeConfigAck, ScopeDeviceRevoke}, DeviceCredential: DeviceCredential{CredentialID: credentialID, Credential: credentialSecret, TokenType: TokenTypeDevice, IssuedAt: credentialIssued, ExpiresAt: credentialExpiry, Scope: []string{ScopeSessionMint, ScopeRotate, ScopeDeviceRevoke}}, Device: toDevice(device), Network: toNetwork(network), SigningKeys: s.SigningKeys(now)}, nil
}

func (s *Service) MintSession(ctx context.Context, credential string, request DeviceSessionRequest) (DeviceSessionResponse, error) {
	if strings.TrimSpace(request.ClientNonce) == "" || len(request.ClientNonce) > 128 {
		return DeviceSessionResponse{}, ErrInvalidInput
	}
	credentialRecord, device, network, err := s.repo.credential(ctx, HashSecret(credential))
	if err != nil || credentialRecord.RevokedAt != nil || !credentialRecord.ExpiresAt.After(s.now()) || device.Status != "active" {
		return DeviceSessionResponse{}, ErrInvalidToken
	}
	now := s.now()
	enrollmentSecret := newOpaqueToken("xenr_")
	expires := canonicalTime(now.Add(s.enrollmentTTL))
	if err := s.repo.DB.WithContext(ctx).Create(&EnrollmentRecord{ID: newID("enr"), DeviceID: device.ID, TokenHash: HashSecret(enrollmentSecret), IssuedAt: canonicalTime(now), ExpiresAt: expires}).Error; err != nil {
		return DeviceSessionResponse{}, err
	}
	return DeviceSessionResponse{ClientNonce: request.ClientNonce, EnrollmentToken: enrollmentSecret, TokenType: TokenTypeBearer, IssuedAt: canonicalTime(now), ExpiresAt: expires, Scope: []string{ScopeConfigRead, ScopeConfigAck}, DeviceID: device.ID, NetworkID: network.ID, SigningKeys: s.SigningKeys(now)}, nil
}

func (s *Service) EnrollmentConfig(ctx context.Context, enrollmentToken, deviceID, networkID string, v2 bool) (SignedConfig, string, error) {
	enrollment, device, network, err := s.repo.enrollment(ctx, HashSecret(enrollmentToken))
	if err != nil || enrollment.ConsumedAt != nil || !enrollment.ExpiresAt.After(s.now()) {
		return SignedConfig{}, "", ErrInvalidToken
	}
	if strings.TrimSpace(deviceID) != device.ID || networkID != "" && networkID != network.ID {
		return SignedConfig{}, "", ErrForbidden
	}
	return s.buildSignedConfigWithContext(ctx, device, network, v2)
}

func (s *Service) UserConfig(ctx context.Context, userID, deviceID, networkID string, v2 bool) (SignedConfig, string, error) {
	device, network, err := s.repo.userDevice(ctx, userID, deviceID)
	if err != nil {
		return SignedConfig{}, "", err
	}
	if networkID != "" && networkID != network.ID {
		return SignedConfig{}, "", ErrForbidden
	}
	return s.buildSignedConfigWithContext(ctx, device, network, v2)
}

func (s *Service) GatewayConfig(ctx context.Context, enrollmentToken string, v2 bool) (GatewaySignedConfig, string, error) {
	enrollment, device, network, err := s.repo.enrollment(ctx, HashSecret(enrollmentToken))
	if err != nil || enrollment.ConsumedAt != nil || !enrollment.ExpiresAt.After(s.now()) || device.Status != "active" {
		return GatewaySignedConfig{}, "", ErrInvalidToken
	}
	if device.Role != RoleGateway || device.ID != network.GatewayID {
		return GatewaySignedConfig{}, "", ErrForbidden
	}
	devices, err := s.repo.devices(ctx, network.ID)
	if err != nil {
		return GatewaySignedConfig{}, "", err
	}
	peers := make([]GatewayPeer, 0, len(devices))
	for _, candidate := range devices {
		if candidate.Role != RoleOne {
			continue
		}
		peers = append(peers, GatewayPeer{DeviceID: candidate.ID, WireGuardPublicKey: candidate.WireGuardPublicKey, WireGuardAddress: candidate.WireGuardAddress, AllowedIPs: candidate.WireGuardAddress})
	}
	now := canonicalTime(s.now())
	address := network.GatewayWireGuardAddress
	if address == "" {
		prefix, parseErr := netip.ParsePrefix(network.CIDR)
		if parseErr != nil {
			return GatewaySignedConfig{}, "", ErrInvalidInput
		}
		address = prefix.Addr().Next().String() + "/32"
	}
	transport := networkTransport(network)
	frontend, listenSocket, err := gatewayFrontend(network)
	if err != nil {
		return GatewaySignedConfig{}, "", err
	}
	schemaVersion := 1
	if v2 {
		schemaVersion = 2
	}
	connectionMode := strings.TrimSpace(network.ConnectionMode)
	if connectionMode == "" {
		connectionMode = DefaultConnectionMode
	}
	config := GatewaySignedConfig{
		SchemaVersion:  schemaVersion,
		Role:           RoleGateway,
		ConfigID:       configID(network.ID, network.GatewayID, network.ConfigGeneration),
		NetworkID:      network.ID,
		GatewayID:      network.GatewayID,
		Generation:     network.ConfigGeneration,
		IssuedAt:       now,
		ExpiresAt:      canonicalTime(now.Add(s.signedConfigTTL)),
		ConnectionMode: connectionMode,
		InterfaceName:  "xconzero0",
		Address:        address,
		ListenPort:     network.GatewayEndpointPort,
		MTU:            1420,
		Peers:          peers,
		Transport:      GatewayTransport{Kind: transport.Kind, ServerName: network.TransportServerName, Port: network.TransportPort, AuthID: network.TransportAuthID, Path: transport.Path, Mode: transport.Mode, Host: transport.Host, Frontend: frontend, ListenSocket: listenSocket},
	}
	if s.meshNetworks[network.ID] {
		config.Mesh, err = s.relaySpec(ctx, network)
		if err != nil {
			return GatewaySignedConfig{}, "", err
		}
	}
	if v2 {
		entry, err := s.cachedPolicy(ctx, network)
		if err != nil {
			return GatewaySignedConfig{}, "", err
		}
		policy := entry.gatewayPolicy
		config.Policy = &policy
	}
	payload, err := gatewaySigningBytes(config)
	if err != nil {
		return GatewaySignedConfig{}, "", err
	}
	config.Signature = Signature{Algorithm: "Ed25519", KeyID: s.keyID, Value: base64.StdEncoding.EncodeToString(ed25519.Sign(s.privateKey, payload))}
	raw, err := json.Marshal(config)
	if err != nil {
		return GatewaySignedConfig{}, "", err
	}
	sum := sha256.Sum256(raw)
	return config, `"` + hex.EncodeToString(sum[:]) + `"`, nil
}

// gatewayFrontend picks the Gateway's Xray frontend. A network that declares
// its own frontend keeps it; otherwise the deployment-wide environment applies,
// so one environment can host both direct-TLS and Caddy-fronted Gateways.
func gatewayFrontend(network NetworkRecord) (string, string, error) {
	if strings.TrimSpace(network.GatewayFrontend) != "" {
		return resolveGatewayFrontend(network.GatewayFrontend, network.GatewayListenSocket)
	}
	return resolveGatewayFrontend(os.Getenv("XCONNECT_GATEWAY_XRAY_FRONTEND"), os.Getenv("XCONNECT_GATEWAY_XRAY_LISTEN_SOCKET"))
}

func resolveGatewayFrontend(frontend, socket string) (string, string, error) {
	frontend = strings.TrimSpace(frontend)
	if frontend == "" {
		frontend = GatewayFrontendDirectTLS
	}
	if frontend != GatewayFrontendDirectTLS && frontend != GatewayFrontendCaddyUnixH2C {
		return "", "", ErrInvalidInput
	}
	if frontend == GatewayFrontendDirectTLS {
		return frontend, "", nil
	}
	socket = strings.TrimSpace(socket)
	if socket == "" {
		socket = DefaultGatewayListenSocket
	}
	if !strings.HasPrefix(socket, "/") || strings.ContainsAny(socket, "\r\n") || len(socket) > 4096 {
		return "", "", ErrInvalidInput
	}
	return frontend, socket, nil
}

func (s *Service) AckEnrollment(ctx context.Context, enrollmentToken string, request SignedConfigAckRequest) (SignedConfigAckResponse, error) {
	enrollment, device, network, err := s.repo.enrollment(ctx, HashSecret(enrollmentToken))
	if err != nil || enrollment.ConsumedAt != nil || !enrollment.ExpiresAt.After(s.now()) {
		return SignedConfigAckResponse{}, ErrInvalidToken
	}
	if request.DeviceID != device.ID || request.ConfigID != configID(network.ID, device.ID, request.Generation) || request.AppliedAt.IsZero() {
		return SignedConfigAckResponse{}, ErrForbidden
	}
	return s.repo.ack(ctx, request, device, network, s.now())
}

func (s *Service) AckUser(ctx context.Context, userID string, request SignedConfigAckRequest) (SignedConfigAckResponse, error) {
	device, network, err := s.repo.userDevice(ctx, userID, request.DeviceID)
	if err != nil {
		return SignedConfigAckResponse{}, err
	}
	if request.ConfigID != configID(network.ID, device.ID, request.Generation) || request.AppliedAt.IsZero() {
		return SignedConfigAckResponse{}, ErrForbidden
	}
	return s.repo.ack(ctx, request, device, network, s.now())
}

func (s *Service) PolicyArtifact(ctx context.Context, enrollmentToken string, generation uint64, digest string) ([]byte, error) {
	enrollment, device, network, err := s.repo.enrollment(ctx, HashSecret(enrollmentToken))
	if err != nil || enrollment.ConsumedAt != nil || !enrollment.ExpiresAt.After(s.now()) {
		return nil, ErrInvalidToken
	}
	if generation != network.ConfigGeneration || device.Status != "active" {
		return nil, ErrForbidden
	}
	entry, err := s.cachedPolicy(ctx, network)
	if err != nil {
		return nil, err
	}
	if entry.clientDigest != digest {
		return nil, ErrNotFound
	}
	return entry.clientRaw, nil
}

func (s *Service) buildSignedConfig(device DeviceRecord, network NetworkRecord, v2 bool) (SignedConfig, string, error) {
	return s.buildSignedConfigWithContext(context.Background(), device, network, v2)
}

func (s *Service) buildSignedConfigWithContext(ctx context.Context, device DeviceRecord, network NetworkRecord, v2 bool) (SignedConfig, string, error) {
	now := canonicalTime(s.now())
	expires := canonicalTime(now.Add(s.signedConfigTTL))
	generation := network.ConfigGeneration
	if generation == 0 {
		generation = 1
	}
	config := SignedConfig{
		SchemaVersion: 1,
		ConfigID:      configID(network.ID, device.ID, generation),
		NetworkID:     network.ID,
		DeviceID:      device.ID,
		Generation:    generation,
		IssuedAt:      now,
		ExpiresAt:     expires,
		ProxyCore:     "xray",
		Transport: Transport{
			Kind: networkTransport(network).Kind, Loopback: Endpoint{Host: "127.0.0.1", Port: s.localProxyPort},
			Remote: RemoteEndpoint{Host: network.GatewayEndpointHost, Port: network.TransportPort, ServerName: network.TransportServerName}, AuthID: network.TransportAuthID,
			Path: networkTransport(network).Path, Mode: networkTransport(network).Mode, Host: networkTransport(network).Host,
		},
		WireGuard: WireGuard{
			InterfaceName: "xconone0", Addresses: []string{device.WireGuardAddress}, MTU: 1420,
			Peers: []WireGuardPeer{{GatewayID: network.GatewayID, PublicKey: network.GatewayWireGuardKey, AllowedIPs: []string{network.CIDR}, Endpoint: Endpoint{Host: "127.0.0.1", Port: s.localProxyPort}, PersistentKeepaliveSeconds: 12}},
		},
	}
	if v2 {
		entry, err := s.cachedPolicy(ctx, network)
		if err != nil {
			return SignedConfig{}, "", err
		}
		digest := entry.clientDigest
		config.SchemaVersion = 2
		config.Policy = &PolicyReference{Generation: generation, Digest: digest, Path: fmt.Sprintf("/api/overlay/v1/enrollment/policy-artifacts/%d/%s", generation, digest), MediaType: PolicyMediaType}
	}
	if v2 && s.meshNetworks[network.ID] {
		mesh, err := s.oneMeshSpec(ctx, device, network)
		config.Mesh = mesh
		if config.Mesh != nil {
			config.WireGuard.MTU = 1280
		}
		if err != nil {
			return SignedConfig{}, "", err
		}
	}
	payload, err := signingBytes(config)
	if err != nil {
		return SignedConfig{}, "", err
	}
	signature := ed25519.Sign(s.privateKey, payload)
	config.Signature = Signature{Algorithm: "Ed25519", KeyID: s.keyID, Value: base64.StdEncoding.EncodeToString(signature)}
	final, err := json.Marshal(config)
	if err != nil {
		return SignedConfig{}, "", err
	}
	etagSum := sha256.Sum256(final)
	return config, `"` + hex.EncodeToString(etagSum[:]) + `"`, nil
}

func defaultPolicyArtifact(networkID string, generation uint64) PolicyArtifact {
	return PolicyArtifact{SchemaVersion: 1, CompilerVersion: PolicyCompilerVersion, NetworkID: networkID, Revision: generation, DefaultAction: "deny", ProtectedFlows: []string{"control:controller-session", "control:gateway-apply-result", "control:gateway-heartbeat", "control:gateway-policy-artifact", "control:gateway-snapshot"}, Rules: []PolicyRule{}}
}

// clientPolicyArtifact projects a network's gateway policy into the strict
// canonical artifact contract expected by XConnect-One's policy.Consume
// (which requires default_action="deny" and does not enforce gateway forwarding
// rules locally). Gateways receive the full PolicyArtifact via GatewayConfig.
func clientPolicyArtifact(policy PolicyArtifact) PolicyArtifact {
	return defaultPolicyArtifact(policy.NetworkID, policy.Revision)
}

func (s *Service) cachedPolicy(ctx context.Context, network NetworkRecord) (cachedPolicyEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hasInlinePolicy := strings.TrimSpace(network.PolicyJSON) != ""
	key := policyCacheKey{networkID: network.ID, generation: network.ConfigGeneration}
	if !hasInlinePolicy {
		s.policyCacheMu.RLock()
		entry, ok := s.policyCache[key]
		s.policyCacheMu.RUnlock()
		if ok {
			return entry, nil
		}
	}
	loaded := network
	if !hasInlinePolicy && s.repo != nil && s.repo.DB != nil {
		dbGen, dbPolicyJSON, err := s.repo.networkPolicyJSON(ctx, network.ID)
		if err != nil {
			return cachedPolicyEntry{}, err
		}
		if dbGen == network.ConfigGeneration {
			loaded.PolicyJSON = dbPolicyJSON
		}
	}
	policy, err := s.policyArtifact(loaded)
	if err != nil {
		return cachedPolicyEntry{}, err
	}
	clientRaw, err := json.Marshal(clientPolicyArtifact(policy))
	if err != nil {
		return cachedPolicyEntry{}, err
	}
	sum := sha256.Sum256(clientRaw)
	entry := cachedPolicyEntry{
		gatewayPolicy: policy,
		clientRaw:     clientRaw,
		clientDigest:  hex.EncodeToString(sum[:]),
	}
	s.policyCacheMu.Lock()
	if s.policyCache == nil {
		s.policyCache = make(map[policyCacheKey]cachedPolicyEntry)
	}
	for existingKey := range s.policyCache {
		if existingKey.networkID == network.ID && existingKey.generation < network.ConfigGeneration {
			delete(s.policyCache, existingKey)
		}
	}
	s.policyCache[key] = entry
	s.policyCacheMu.Unlock()
	return entry, nil
}

func (s *Service) evictPolicyCache(networkID string) {
	s.policyCacheMu.Lock()
	for key := range s.policyCache {
		if key.networkID == networkID {
			delete(s.policyCache, key)
		}
	}
	s.policyCacheMu.Unlock()
}

func (s *Service) policyArtifact(network NetworkRecord) (PolicyArtifact, error) {
	if strings.TrimSpace(network.PolicyJSON) == "" {
		return defaultPolicyArtifact(network.ID, network.ConfigGeneration), nil
	}
	var policy PolicyArtifact
	if err := json.Unmarshal([]byte(network.PolicyJSON), &policy); err != nil {
		return PolicyArtifact{}, fmt.Errorf("decode stored overlay policy: %w", err)
	}
	if policy.NetworkID != network.ID || policy.Revision != network.ConfigGeneration {
		return PolicyArtifact{}, ErrGenerationConflict
	}
	return policy, nil
}

func networkTransport(network NetworkRecord) Transport {
	kind, path, mode := network.TransportKind, network.TransportPath, network.TransportMode
	if strings.TrimSpace(kind) == "" {
		kind = TransportVLESSXHTTP
	}
	if strings.TrimSpace(path) == "" {
		path = DefaultTransportPath
	}
	if strings.TrimSpace(mode) == "" {
		mode = DefaultTransportMode
	}
	return Transport{Kind: kind, Path: path, Mode: mode, Host: network.TransportHost}
}

func signingBytes(config SignedConfig) ([]byte, error) {
	unsigned := struct {
		SchemaVersion int              `json:"schema_version"`
		ConfigID      string           `json:"config_id"`
		NetworkID     string           `json:"network_id"`
		DeviceID      string           `json:"device_id"`
		Generation    uint64           `json:"generation"`
		IssuedAt      time.Time        `json:"issued_at"`
		ExpiresAt     time.Time        `json:"expires_at"`
		ProxyCore     string           `json:"proxy_core"`
		Transport     Transport        `json:"transport"`
		WireGuard     WireGuard        `json:"wireguard"`
		Policy        *PolicyReference `json:"policy"`
		Mesh          *MeshSpec        `json:"mesh,omitempty"`
	}{config.SchemaVersion, config.ConfigID, config.NetworkID, config.DeviceID, config.Generation, config.IssuedAt, config.ExpiresAt, config.ProxyCore, config.Transport, config.WireGuard, config.Policy, config.Mesh}
	if config.SchemaVersion == 1 {
		return json.Marshal(struct {
			SchemaVersion int       `json:"schema_version"`
			ConfigID      string    `json:"config_id"`
			NetworkID     string    `json:"network_id"`
			DeviceID      string    `json:"device_id"`
			Generation    uint64    `json:"generation"`
			IssuedAt      time.Time `json:"issued_at"`
			ExpiresAt     time.Time `json:"expires_at"`
			ProxyCore     string    `json:"proxy_core"`
			Transport     Transport `json:"transport"`
			WireGuard     WireGuard `json:"wireguard"`
		}{config.SchemaVersion, config.ConfigID, config.NetworkID, config.DeviceID, config.Generation, config.IssuedAt, config.ExpiresAt, config.ProxyCore, config.Transport, config.WireGuard})
	}
	return json.Marshal(unsigned)
}

func gatewaySigningBytes(config GatewaySignedConfig) ([]byte, error) {
	connectionMode := strings.TrimSpace(config.ConnectionMode)
	if config.SchemaVersion == 1 {
		return json.Marshal(struct {
			SchemaVersion  int              `json:"schema_version"`
			Role           string           `json:"role"`
			ConfigID       string           `json:"config_id"`
			NetworkID      string           `json:"network_id"`
			GatewayID      string           `json:"gateway_id"`
			Generation     uint64           `json:"generation"`
			IssuedAt       time.Time        `json:"issued_at"`
			ExpiresAt      time.Time        `json:"expires_at"`
			ConnectionMode string           `json:"connection_mode,omitempty"`
			InterfaceName  string           `json:"interface_name"`
			Address        string           `json:"address"`
			ListenPort     int              `json:"listen_port"`
			MTU            int              `json:"mtu"`
			Peers          []GatewayPeer    `json:"peers"`
			Transport      GatewayTransport `json:"transport"`
			Mesh           *RelaySpec       `json:"mesh,omitempty"`
		}{config.SchemaVersion, config.Role, config.ConfigID, config.NetworkID, config.GatewayID, config.Generation, config.IssuedAt, config.ExpiresAt, connectionMode, config.InterfaceName, config.Address, config.ListenPort, config.MTU, config.Peers, config.Transport, config.Mesh})
	}
	return json.Marshal(struct {
		SchemaVersion  int              `json:"schema_version"`
		Role           string           `json:"role"`
		ConfigID       string           `json:"config_id"`
		NetworkID      string           `json:"network_id"`
		GatewayID      string           `json:"gateway_id"`
		Generation     uint64           `json:"generation"`
		IssuedAt       time.Time        `json:"issued_at"`
		ExpiresAt      time.Time        `json:"expires_at"`
		ConnectionMode string           `json:"connection_mode,omitempty"`
		InterfaceName  string           `json:"interface_name"`
		Address        string           `json:"address"`
		ListenPort     int              `json:"listen_port"`
		MTU            int              `json:"mtu"`
		Peers          []GatewayPeer    `json:"peers"`
		Transport      GatewayTransport `json:"transport"`
		Mesh           *RelaySpec       `json:"mesh,omitempty"`
		Policy         *PolicyArtifact  `json:"policy,omitempty"`
	}{config.SchemaVersion, config.Role, config.ConfigID, config.NetworkID, config.GatewayID, config.Generation, config.IssuedAt, config.ExpiresAt, connectionMode, config.InterfaceName, config.Address, config.ListenPort, config.MTU, config.Peers, config.Transport, config.Mesh, config.Policy})
}

func validateExchange(request ExchangeRequest) error {
	role := request.Role
	if role == "" {
		role = RoleOne
	}
	publicKey, keyErr := base64.StdEncoding.DecodeString(request.WireGuardPublicKey)
	if !validOpaque(request.JoinToken, "xjt_") || !overlayDeviceIDPattern.MatchString(request.DeviceID) || strings.TrimSpace(request.Platform) == "" || keyErr != nil || len(publicKey) != 32 || len(request.Name) > 255 || len(request.Hostname) > 255 {
		return ErrInvalidInput
	}
	switch request.Platform {
	case "linux", "darwin", "windows", "ios", "android":
	default:
		return ErrInvalidInput
	}
	if role != RoleOne && role != RoleGateway {
		return ErrInvalidInput
	}
	return nil
}

func validOpaque(value, prefix string) bool {
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, prefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(raw) == 32
}

func canonicalTime(value time.Time) time.Time { return value.UTC().Truncate(time.Second) }
func (s *Service) now() time.Time             { return canonicalTime(s.clock()) }

func newOpaqueToken(prefix string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf)
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func newID(prefix string) string { return prefix + "_" + strings.ReplaceAll(uuidNewString(), "-", "") }
func uuidNewString() string      { return fmt.Sprintf("%s", randomUUID()) }
func randomUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", buf[:4], buf[4:6], buf[6:8], buf[8:10], buf[10:])
}

func configID(networkID, deviceID string, generation uint64) string {
	sum := sha256.Sum256([]byte(networkID + "\x00" + deviceID + "\x00" + fmt.Sprint(generation)))
	return "cfg_" + hex.EncodeToString(sum[:])[:32]
}

func toDevice(record DeviceRecord) Device {
	role := ""
	if record.Role == RoleGateway {
		role = RoleGateway
	}
	return Device{ID: record.ID, UserID: record.UserID, NetworkID: record.NetworkID, Role: role, Name: record.Name, Platform: record.Platform, Hostname: record.Hostname, WireGuardPublicKey: record.WireGuardPublicKey, WireGuardAddress: record.WireGuardAddress, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(), LastSeenAt: record.LastSeenAt}
}

func toAdminDevice(record DeviceRecord, network NetworkRecord, ackReceivedAt time.Time, acknowledged bool, hasAnyACK bool, telemetry *DeviceTelemetryRecord, now time.Time) AdminDevice {
	connectionStatus := "never_seen"
	if record.Status == "revoked" {
		connectionStatus = "revoked"
	} else if acknowledged {
		connectionStatus = "stale"
		lastACKAt := ackReceivedAt
		if record.LastSeenAt != nil && record.LastSeenAt.After(lastACKAt) {
			lastACKAt = *record.LastSeenAt
		}
		if !lastACKAt.After(now) && !lastACKAt.Before(now.Add(-adminDeviceACKRecentWindow)) {
			connectionStatus = "recent_ack"
		}
	} else if telemetry != nil {
		connectionStatus = "stale"
		observedAt := telemetry.UpdatedAt
		if record.LastSeenAt != nil && record.LastSeenAt.After(observedAt) {
			observedAt = *record.LastSeenAt
		}
		if !observedAt.After(now) && !observedAt.Before(now.Add(-adminDeviceACKRecentWindow)) {
			connectionStatus = "recent_ack"
		}
	} else if !hasAnyACK && record.LastSeenAt != nil {
		connectionStatus = "stale"
		observedAt := *record.LastSeenAt
		if !observedAt.After(now) && !observedAt.Before(now.Add(-adminDeviceACKRecentWindow)) {
			connectionStatus = "recent_ack"
		}
	}
	adminDevice := AdminDevice{
		ID:                 record.ID,
		UserID:             record.UserID,
		NetworkID:          record.NetworkID,
		Role:               record.Role,
		Name:               record.Name,
		Platform:           record.Platform,
		Hostname:           record.Hostname,
		WireGuardPublicKey: record.WireGuardPublicKey,
		WireGuardAddress:   record.WireGuardAddress,
		Status:             record.Status,
		LastSeenAt:         record.LastSeenAt,
		ConnectionStatus:   connectionStatus,
		CreatedAt:          record.CreatedAt.UTC(),
		UpdatedAt:          record.UpdatedAt.UTC(),
	}
	if telemetry != nil {
		healthy := telemetry.Healthy
		adminDevice.RuntimeHealthy = &healthy
		adminDevice.ActivePath = telemetry.ActivePath
		if telemetry.RTTMillis > 0 {
			rtt := telemetry.RTTMillis
			adminDevice.RTTMillis = &rtt
		}
		adminDevice.RxBytes = telemetry.RxBytes
		adminDevice.TxBytes = telemetry.TxBytes
		adminDevice.AppliedGeneration = telemetry.AppliedGeneration
		mode := strings.TrimSpace(telemetry.ConnectionMode)
		if mode == "" {
			mode = strings.TrimSpace(network.ConnectionMode)
		}
		adminDevice.ConnectionMode = mode
	}
	return adminDevice
}

func toNetwork(record NetworkRecord) Network {
	mode := strings.TrimSpace(record.ConnectionMode)
	if mode == "" {
		mode = DefaultConnectionMode
	}
	return Network{
		ID:                  record.ID,
		DisplayName:         record.DisplayName,
		CIDR:                record.CIDR,
		GatewayID:           record.GatewayID,
		GatewayWireGuardKey: record.GatewayWireGuardKey,
		GatewayEndpointHost: record.GatewayEndpointHost,
		GatewayEndpointPort: record.GatewayEndpointPort,
		TransportServerName: record.TransportServerName,
		TransportPort:       record.TransportPort,
		TransportAuthID:     record.TransportAuthID,
		TransportKind:       record.TransportKind,
		TransportPath:       record.TransportPath,
		TransportMode:       record.TransportMode,
		TransportHost:       record.TransportHost,
		GatewayFrontend:     record.GatewayFrontend,
		GatewayListenSocket: record.GatewayListenSocket,
		ConnectionMode:      mode,
		ConfigGeneration:    record.ConfigGeneration,
		CreatedAt:           record.CreatedAt.UTC(),
		UpdatedAt:           record.UpdatedAt.UTC(),
	}
}
