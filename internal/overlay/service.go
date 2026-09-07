package overlay

import (
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
	"os"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
)

var overlayDeviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type Service struct {
	repo            *Repository
	keyID           string
	privateKey      ed25519.PrivateKey
	enrollmentTTL   time.Duration
	credentialTTL   time.Duration
	signedConfigTTL time.Duration
	clock           func() time.Time
}

func NewService(db *gorm.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("overlay service requires a database")
	}
	if err := AutoMigrate(db); err != nil {
		return nil, fmt.Errorf("migrate overlay schema: %w", err)
	}
	cfg = cfg.withDefaults()
	key := append(ed25519.PrivateKey(nil), cfg.SigningPrivateKey...)
	if len(key) == 0 {
		var err error
		_, key, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate overlay signing key: %w", err)
		}
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("overlay signing key must be an Ed25519 private key")
	}
	repo, err := NewRepository(db)
	if err != nil {
		return nil, err
	}
	return &Service{repo: repo, keyID: cfg.SigningKeyID, privateKey: key, enrollmentTTL: cfg.EnrollmentTTL, credentialTTL: cfg.CredentialTTL, signedConfigTTL: cfg.SignedConfigTTL, clock: cfg.Clock}, nil
}

// ConfigFromEnv is intended for the account service entrypoint. The private
// key is injected by the deployment secret manager (Vault in production),
// encoded as unpadded or padded base64. Development may omit it and receives
// an ephemeral key, which makes the limitation explicit at the boundary.
func ConfigFromEnv() (Config, error) {
	keyText := strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY"))
	if keyText == "" {
		return Config{SigningKeyID: strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_SIGNING_KEY_ID"))}, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(keyText)
	}
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return Config{}, errors.New("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY must be base64 Ed25519 private key")
	}
	return Config{SigningKeyID: strings.TrimSpace(os.Getenv("XCONNECT_OVERLAY_SIGNING_KEY_ID")), SigningPrivateKey: ed25519.PrivateKey(key)}, nil
}

func (s *Service) Repository() *Repository { return s.repo }

func (s *Service) Seed(ctx context.Context, cfg BootstrapConfig, joinToken string) (string, error) {
	return s.repo.Seed(ctx, cfg, joinToken)
}

func (s *Service) SigningKeys(now time.Time) []SigningKey {
	now = canonicalTime(now)
	return []SigningKey{{KeyID: s.keyID, Algorithm: "Ed25519", PublicKey: base64.StdEncoding.EncodeToString(s.privateKey.Public().(ed25519.PublicKey)), Status: "current", NotBefore: now.Add(-time.Minute)}}
}

func (s *Service) Exchange(ctx context.Context, request ExchangeRequest) (ExchangeResponse, error) {
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
	credentialSecret := newOpaqueToken("xdc_")
	enrollmentSecret := newOpaqueToken("xenr_")
	credentialIssued := canonicalTime(now)
	credentialExpiry := canonicalTime(now.Add(s.credentialTTL))
	enrollmentExpiry := canonicalTime(now.Add(s.enrollmentTTL))
	credentialID := "xdcid_" + randomHex(16)
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
	return s.buildSignedConfig(device, network, v2)
}

func (s *Service) UserConfig(ctx context.Context, userID, deviceID, networkID string, v2 bool) (SignedConfig, string, error) {
	device, network, err := s.repo.userDevice(ctx, userID, deviceID)
	if err != nil {
		return SignedConfig{}, "", err
	}
	if networkID != "" && networkID != network.ID {
		return SignedConfig{}, "", ErrForbidden
	}
	return s.buildSignedConfig(device, network, v2)
}

func (s *Service) GatewayConfig(ctx context.Context, enrollmentToken string) (GatewaySignedConfig, string, error) {
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
	config := GatewaySignedConfig{SchemaVersion: 1, Role: RoleGateway, ConfigID: configID(network.ID, network.GatewayID, network.ConfigGeneration), NetworkID: network.ID, GatewayID: network.GatewayID, Generation: network.ConfigGeneration, IssuedAt: now, ExpiresAt: canonicalTime(now.Add(s.signedConfigTTL)), InterfaceName: "xconzero0", Address: address, ListenPort: network.GatewayEndpointPort, MTU: 1420, Peers: peers, Transport: GatewayTransport{ServerName: network.TransportServerName, Port: network.TransportPort, AuthID: network.TransportAuthID}}
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
	raw, err := json.Marshal(defaultPolicyArtifact(network.ID, generation))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, ErrNotFound
	}
	return raw, nil
}

func (s *Service) buildSignedConfig(device DeviceRecord, network NetworkRecord, v2 bool) (SignedConfig, string, error) {
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
			Kind: "vless-tls-xudp", Loopback: Endpoint{Host: "127.0.0.1", Port: 18080},
			Remote: RemoteEndpoint{Host: network.GatewayEndpointHost, Port: network.TransportPort, ServerName: network.TransportServerName}, AuthID: network.TransportAuthID,
		},
		WireGuard: WireGuard{
			InterfaceName: "xconone0", Addresses: []string{device.WireGuardAddress}, MTU: 1420,
			Peers: []WireGuardPeer{{GatewayID: network.GatewayID, PublicKey: network.GatewayWireGuardKey, AllowedIPs: []string{network.CIDR}, Endpoint: Endpoint{Host: "127.0.0.1", Port: 51820}, PersistentKeepaliveSeconds: 25}},
		},
	}
	if v2 {
		raw, err := json.Marshal(defaultPolicyArtifact(network.ID, generation))
		if err != nil {
			return SignedConfig{}, "", err
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		config.SchemaVersion = 2
		config.Policy = &PolicyReference{Generation: generation, Digest: digest, Path: fmt.Sprintf("/api/overlay/v1/enrollment/policy-artifacts/%d/%s", generation, digest), MediaType: PolicyMediaType}
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
	}{config.SchemaVersion, config.ConfigID, config.NetworkID, config.DeviceID, config.Generation, config.IssuedAt, config.ExpiresAt, config.ProxyCore, config.Transport, config.WireGuard, config.Policy}
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
	return json.Marshal(struct {
		SchemaVersion int              `json:"schema_version"`
		Role          string           `json:"role"`
		ConfigID      string           `json:"config_id"`
		NetworkID     string           `json:"network_id"`
		GatewayID     string           `json:"gateway_id"`
		Generation    uint64           `json:"generation"`
		IssuedAt      time.Time        `json:"issued_at"`
		ExpiresAt     time.Time        `json:"expires_at"`
		InterfaceName string           `json:"interface_name"`
		Address       string           `json:"address"`
		ListenPort    int              `json:"listen_port"`
		MTU           int              `json:"mtu"`
		Peers         []GatewayPeer    `json:"peers"`
		Transport     GatewayTransport `json:"transport"`
	}{config.SchemaVersion, config.Role, config.ConfigID, config.NetworkID, config.GatewayID, config.Generation, config.IssuedAt, config.ExpiresAt, config.InterfaceName, config.Address, config.ListenPort, config.MTU, config.Peers, config.Transport})
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

func toNetwork(record NetworkRecord) Network {
	return Network{ID: record.ID, DisplayName: record.DisplayName, CIDR: record.CIDR, GatewayID: record.GatewayID, GatewayWireGuardKey: record.GatewayWireGuardKey, GatewayEndpointHost: record.GatewayEndpointHost, GatewayEndpointPort: record.GatewayEndpointPort, TransportServerName: record.TransportServerName, TransportPort: record.TransportPort, TransportAuthID: record.TransportAuthID, ConfigGeneration: record.ConfigGeneration, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC()}
}
