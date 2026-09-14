package overlay

import (
	"crypto/ed25519"
	"errors"
	"net/netip"
	"strings"
	"time"
)

const (
	RoleGateway = "gateway"
	RoleOne     = "one"

	TransportVLESSXHTTP  = "vless-xhttp"
	DefaultTransportPath = "/xconnect"
	DefaultTransportMode = "auto"

	TokenTypeBearer = "Bearer"
	TokenTypeDevice = "Device"

	ScopeConfigRead   = "overlay:config:read"
	ScopeConfigAck    = "overlay:config:ack"
	ScopeDeviceRevoke = "overlay:device:revoke"
	ScopeSessionMint  = "overlay:session:mint"
	ScopeRotate       = "overlay:credential:rotate"

	SignedConfigV2MediaType = "application/vnd.xconnect.signed-config.v2+json"
	PolicyMediaType         = "application/vnd.xconnect.policy.v1+json"
	PolicyCompilerVersion   = "xconnect-acl-v1alpha1.1"
)

var (
	ErrInvalidToken             = errors.New("invalid or expired overlay token")
	ErrInviteConstraint         = errors.New("overlay invite constraints do not match")
	ErrDeviceConflict           = errors.New("overlay device registration conflicts")
	ErrNotFound                 = errors.New("overlay resource not found")
	ErrGenerationConflict       = errors.New("overlay configuration generation conflict")
	ErrForbidden                = errors.New("overlay resource access denied")
	ErrInvalidInput             = errors.New("invalid overlay request")
	ErrRegistrationNotAvailable = errors.New("overlay registration is not available")
	ErrRegistrationLimited      = errors.New("overlay registration is limited")
	ErrInvalidRegistrationToken = errors.New("invalid overlay registration token")
	ErrRegistrationPending      = errors.New("overlay registration is pending")
	ErrRegistrationRejected     = errors.New("overlay registration was rejected")
	ErrRegistrationExpired      = errors.New("overlay registration expired")
	ErrRegistrationConsumed     = errors.New("overlay registration was consumed")
	ErrRegistrationNotPending   = errors.New("overlay registration is not pending")
)

type Network struct {
	ID                  string    `json:"id"`
	DisplayName         string    `json:"display_name"`
	CIDR                string    `json:"cidr"`
	GatewayID           string    `json:"gateway_id"`
	GatewayWireGuardKey string    `json:"gateway_wireguard_public_key"`
	GatewayEndpointHost string    `json:"gateway_endpoint_host"`
	GatewayEndpointPort int       `json:"gateway_endpoint_port"`
	TransportServerName string    `json:"transport_server_name"`
	TransportPort       int       `json:"transport_port"`
	TransportAuthID     string    `json:"transport_auth_id"`
	TransportKind       string    `json:"transport_kind"`
	TransportPath       string    `json:"transport_path,omitempty"`
	TransportMode       string    `json:"transport_mode,omitempty"`
	TransportHost       string    `json:"transport_host,omitempty"`
	ConfigGeneration    uint64    `json:"-"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type Device struct {
	ID                 string     `json:"id"`
	UserID             string     `json:"user_id,omitempty"`
	NetworkID          string     `json:"network_id"`
	Role               string     `json:"role,omitempty"`
	Name               string     `json:"name"`
	Platform           string     `json:"platform"`
	Hostname           string     `json:"hostname"`
	WireGuardPublicKey string     `json:"wireguard_public_key"`
	WireGuardAddress   string     `json:"wireguard_address"`
	CreatedAt          time.Time  `json:"created_at,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at,omitempty"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
}

// AdminDevice is the owner-scoped read model for the Portal. It is separate
// from Device because enrollment and exchange responses intentionally keep a
// strict, backwards-compatible contract.
type AdminDevice struct {
	ID                 string     `json:"id"`
	UserID             string     `json:"user_id,omitempty"`
	NetworkID          string     `json:"network_id"`
	Role               string     `json:"role"`
	Name               string     `json:"name"`
	Platform           string     `json:"platform"`
	Hostname           string     `json:"hostname"`
	WireGuardPublicKey string     `json:"wireguard_public_key"`
	WireGuardAddress   string     `json:"wireguard_address"`
	Status             string     `json:"status"`
	LastSeenAt         *time.Time `json:"last_seen_at"`
	ConnectionStatus   string     `json:"connection_status"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type SigningKey struct {
	KeyID     string     `json:"key_id"`
	Algorithm string     `json:"algorithm"`
	PublicKey string     `json:"public_key"`
	Status    string     `json:"status"`
	NotBefore time.Time  `json:"not_before"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type RemoteEndpoint struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ServerName string `json:"server_name"`
}

type Transport struct {
	Kind     string         `json:"kind"`
	Loopback Endpoint       `json:"loopback"`
	Remote   RemoteEndpoint `json:"remote"`
	AuthID   string         `json:"auth_id"`
	Path     string         `json:"path,omitempty"`
	Mode     string         `json:"mode,omitempty"`
	Host     string         `json:"host,omitempty"`
}

type WireGuardPeer struct {
	GatewayID                  string   `json:"gateway_id"`
	PublicKey                  string   `json:"public_key"`
	AllowedIPs                 []string `json:"allowed_ips"`
	Endpoint                   Endpoint `json:"endpoint"`
	PersistentKeepaliveSeconds int      `json:"persistent_keepalive_seconds,omitempty"`
}

type WireGuard struct {
	InterfaceName string          `json:"interface_name"`
	Addresses     []string        `json:"addresses"`
	MTU           int             `json:"mtu"`
	Peers         []WireGuardPeer `json:"peers"`
}

type PolicyReference struct {
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest"`
	Path       string `json:"path"`
	MediaType  string `json:"media_type"`
}

type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

type SignedConfig struct {
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
	Policy        *PolicyReference `json:"policy,omitempty"`
	Signature     Signature        `json:"signature"`
}

type DeviceCredential struct {
	CredentialID string    `json:"credential_id"`
	Credential   string    `json:"credential"`
	TokenType    string    `json:"token_type"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        []string  `json:"scope"`
}

type ExchangeRequest struct {
	JoinToken          string `json:"join_token"`
	DeviceID           string `json:"device_id"`
	Name               string `json:"name,omitempty"`
	Platform           string `json:"platform"`
	Hostname           string `json:"hostname,omitempty"`
	WireGuardPublicKey string `json:"wireguard_public_key"`
	Role               string `json:"role,omitempty"`
}

type ExchangeResponse struct {
	EnrollmentToken  string           `json:"enrollment_token"`
	TokenType        string           `json:"token_type"`
	ExpiresAt        time.Time        `json:"expires_at"`
	Scope            []string         `json:"scope"`
	DeviceCredential DeviceCredential `json:"device_credential"`
	Device           Device           `json:"device"`
	Network          Network          `json:"network"`
	SigningKeys      []SigningKey     `json:"signing_keys"`
}

const (
	RegistrationStatusPending  = "pending"
	RegistrationStatusApproved = "approved"
	RegistrationStatusRejected = "rejected"
	RegistrationStatusExpired  = "expired"
	RegistrationStatusConsumed = "consumed"
)

// RegistrationRequest is intentionally unauthenticated. It is only a request
// for an owner to review a declared One identity; it grants no device, route,
// credential, peer, or configuration until an owner approves and the device
// performs a separately authenticated exchange.
type RegistrationRequest struct {
	NetworkID          string `json:"network_id"`
	DeviceID           string `json:"device_id"`
	Name               string `json:"name,omitempty"`
	Hostname           string `json:"hostname,omitempty"`
	Platform           string `json:"platform"`
	WireGuardPublicKey string `json:"wireguard_public_key"`
}

type RegistrationResponse struct {
	RegistrationID    string    `json:"registration_id"`
	RegistrationToken string    `json:"registration_token"`
	Status            string    `json:"status"`
	ExpiresAt         time.Time `json:"expires_at"`
	Interval          int       `json:"interval"`
}

type RegistrationPendingResponse struct {
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	Interval  int       `json:"interval"`
}

// RegistrationSummary is owner-scoped, public identity metadata. It never
// includes a registration token, token digest, enrollment token, credential,
// peer, configuration, IP address, or WireGuard private key.
type RegistrationSummary struct {
	RegistrationID                string     `json:"registration_id"`
	NetworkID                     string     `json:"network_id"`
	DeviceID                      string     `json:"device_id"`
	Name                          string     `json:"name"`
	Hostname                      string     `json:"hostname"`
	Platform                      string     `json:"platform"`
	WireGuardPublicKeyFingerprint string     `json:"wireguard_public_key_fingerprint"`
	Status                        string     `json:"status"`
	ExpiresAt                     time.Time  `json:"expires_at"`
	CreatedAt                     time.Time  `json:"created_at"`
	ApprovedAt                    *time.Time `json:"approved_at,omitempty"`
	RejectedAt                    *time.Time `json:"rejected_at,omitempty"`
	ConsumedAt                    *time.Time `json:"consumed_at,omitempty"`
}

// RegistrationPage is deliberately bounded so an owner cannot accidentally
// request an unbounded audit history from the Portal.
type RegistrationPage struct {
	Registrations []RegistrationSummary `json:"registrations"`
	HasMore       bool                  `json:"has_more"`
	NextCursor    string                `json:"next_cursor,omitempty"`
}

type ApproveRegistrationRequest struct {
	NetworkID string `json:"network_id"`
}

type DeviceSessionRequest struct {
	ClientNonce string `json:"client_nonce"`
}

type DeviceSessionResponse struct {
	ClientNonce     string       `json:"client_nonce"`
	EnrollmentToken string       `json:"enrollment_token"`
	TokenType       string       `json:"token_type"`
	IssuedAt        time.Time    `json:"issued_at"`
	ExpiresAt       time.Time    `json:"expires_at"`
	Scope           []string     `json:"scope"`
	DeviceID        string       `json:"device_id"`
	NetworkID       string       `json:"network_id"`
	SigningKeys     []SigningKey `json:"signing_keys"`
}

type SignedConfigAckRequest struct {
	Generation uint64    `json:"-"`
	ConfigID   string    `json:"config_id"`
	DeviceID   string    `json:"device_id"`
	AppliedAt  time.Time `json:"applied_at"`
}

type SignedConfigAck struct {
	DeviceID   string    `json:"device_id"`
	ConfigID   string    `json:"config_id"`
	Generation uint64    `json:"generation"`
	AppliedAt  time.Time `json:"applied_at"`
	ReceivedAt time.Time `json:"received_at"`
}

type SignedConfigAckResponse struct {
	Acked     bool            `json:"acked"`
	Duplicate bool            `json:"duplicate"`
	Ack       SignedConfigAck `json:"ack"`
}

type GatewayPeer struct {
	DeviceID           string `json:"device_id"`
	WireGuardPublicKey string `json:"wireguard_public_key"`
	WireGuardAddress   string `json:"wireguard_address"`
	AllowedIPs         string `json:"allowed_ips"`
}

// GatewaySignedConfig is the relay/service role's centrally signed snapshot.
// It is a separate wire type because a Gateway installs peers and forwarding
// policy, while One installs a local client tunnel and Xray transport.
type GatewaySignedConfig struct {
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
	Signature     Signature        `json:"signature"`
}

type GatewayTransport struct {
	Kind       string `json:"kind"`
	ServerName string `json:"server_name"`
	Port       int    `json:"port"`
	AuthID     string `json:"auth_id"`
	Path       string `json:"path,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Host       string `json:"host,omitempty"`
}

type PolicyArtifact struct {
	SchemaVersion   int          `json:"schema_version"`
	CompilerVersion string       `json:"compiler_version"`
	NetworkID       string       `json:"network_id"`
	Revision        uint64       `json:"revision"`
	DefaultAction   string       `json:"default_action"`
	ProtectedFlows  []string     `json:"protected_flows"`
	Rules           []PolicyRule `json:"rules"`
}

type PolicyRule struct {
	ID                 string   `json:"id"`
	Action             string   `json:"action"`
	SourceDevices      []string `json:"source_devices"`
	DestinationDevices []string `json:"destination_devices"`
	Protocols          []string `json:"protocols"`
	Ports              []int    `json:"ports"`
}

type AdminOverview struct {
	Status        string `json:"status"`
	NetworkCount  int64  `json:"networkCount"`
	DeviceCount   int64  `json:"deviceCount"`
	GatewayCount  int64  `json:"gatewayCount"`
	OneCount      int64  `json:"oneCount"`
	GatewayStatus string `json:"gatewayStatus"`
	OneStatus     string `json:"oneStatus"`
	SigningKeyID  string `json:"signingKeyId"`
}

type InviteSummary struct {
	ID            string     `json:"id"`
	NetworkID     string     `json:"network_id"`
	DeviceID      string     `json:"device_id,omitempty"`
	Platform      string     `json:"platform"`
	Role          string     `json:"role"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RemainingUses int        `json:"remaining_uses"`
	ConsumedAt    *time.Time `json:"consumed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type AdminBootstrapResult struct {
	Network Network       `json:"network"`
	Invite  InviteSummary `json:"invite"`
	JoinURI string        `json:"join_uri"`
}

// AdminInviteRequest creates one device-bound enrollment invitation for an
// existing owner-scoped network. A raw token is returned only once in JoinURI.
type AdminInviteRequest struct {
	ControllerURL string    `json:"controller_url"`
	NetworkID     string    `json:"network_id"`
	DeviceID      string    `json:"device_id"`
	Platform      string    `json:"platform"`
	Role          string    `json:"role"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type AdminInviteResult struct {
	Invite  InviteSummary `json:"invite"`
	JoinURI string        `json:"join_uri"`
}

type BootstrapConfig struct {
	Network BootstrapNetwork
	Invite  BootstrapInvite
}

type BootstrapNetwork struct {
	ID                      string
	DisplayName             string
	CIDR                    string
	GatewayID               string
	GatewayWireGuardKey     string
	GatewayWireGuardAddress string
	GatewayEndpointHost     string
	GatewayEndpointPort     int
	TransportServerName     string
	TransportPort           int
	TransportAuthID         string
	TransportKind           string
	TransportPath           string
	TransportMode           string
	TransportHost           string
	OwnerUserID             string
}

type BootstrapInvite struct {
	Token     string
	DeviceID  string
	Platform  string
	Role      string
	ExpiresAt time.Time
}

type Config struct {
	SigningKeyID      string
	SigningPrivateKey ed25519.PrivateKey
	EnrollmentTTL     time.Duration
	CredentialTTL     time.Duration
	SignedConfigTTL   time.Duration
	Clock             func() time.Time
}

func (c Config) withDefaults() Config {
	if c.SigningKeyID == "" {
		c.SigningKeyID = "zero-signing-current"
	}
	if c.EnrollmentTTL <= 0 || c.EnrollmentTTL > 15*time.Minute {
		c.EnrollmentTTL = 15 * time.Minute
	}
	if c.CredentialTTL <= 0 || c.CredentialTTL > 31*24*time.Hour {
		c.CredentialTTL = 30 * 24 * time.Hour
	}
	if c.SignedConfigTTL <= 0 || c.SignedConfigTTL > 15*time.Minute {
		c.SignedConfigTTL = 10 * time.Minute
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

func (n BootstrapNetwork) validate() error {
	if strings.TrimSpace(n.ID) == "" || strings.TrimSpace(n.CIDR) == "" || strings.TrimSpace(n.GatewayID) == "" || strings.TrimSpace(n.GatewayWireGuardKey) == "" || strings.TrimSpace(n.GatewayEndpointHost) == "" || n.GatewayEndpointPort < 1 || n.GatewayEndpointPort > 65535 || strings.TrimSpace(n.TransportServerName) == "" || n.TransportPort != 443 || strings.TrimSpace(n.TransportAuthID) == "" {
		return ErrInvalidInput
	}
	if kind := strings.TrimSpace(n.TransportKind); kind != "" && kind != TransportVLESSXHTTP {
		return ErrInvalidInput
	}
	if path := strings.TrimSpace(n.TransportPath); path != "" && (!strings.HasPrefix(path, "/") || len(path) > 1024) {
		return ErrInvalidInput
	}
	if mode := strings.TrimSpace(n.TransportMode); mode != "" && mode != DefaultTransportMode && mode != "packet-up" && mode != "stream-up" {
		return ErrInvalidInput
	}
	if _, err := netip.ParsePrefix(n.CIDR); err != nil {
		return ErrInvalidInput
	}
	return nil
}

func (n BootstrapNetwork) withTransportDefaults() BootstrapNetwork {
	if strings.TrimSpace(n.TransportKind) == "" {
		n.TransportKind = TransportVLESSXHTTP
	}
	if strings.TrimSpace(n.TransportPath) == "" {
		n.TransportPath = DefaultTransportPath
	}
	if strings.TrimSpace(n.TransportMode) == "" {
		n.TransportMode = DefaultTransportMode
	}
	return n
}
