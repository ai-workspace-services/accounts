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
	ErrInvalidToken       = errors.New("invalid or expired overlay token")
	ErrInviteConstraint   = errors.New("overlay invite constraints do not match")
	ErrDeviceConflict     = errors.New("overlay device registration conflicts")
	ErrNotFound           = errors.New("overlay resource not found")
	ErrGenerationConflict = errors.New("overlay configuration generation conflict")
	ErrForbidden          = errors.New("overlay resource access denied")
	ErrInvalidInput       = errors.New("invalid overlay request")
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
	ServerName string `json:"server_name"`
	Port       int    `json:"port"`
	AuthID     string `json:"auth_id"`
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
	if strings.TrimSpace(n.ID) == "" || strings.TrimSpace(n.CIDR) == "" || strings.TrimSpace(n.GatewayID) == "" || strings.TrimSpace(n.GatewayWireGuardKey) == "" || strings.TrimSpace(n.GatewayEndpointHost) == "" || n.GatewayEndpointPort < 1 || n.GatewayEndpointPort > 65535 || strings.TrimSpace(n.TransportServerName) == "" || n.TransportPort < 1 || n.TransportPort > 65535 || strings.TrimSpace(n.TransportAuthID) == "" {
		return ErrInvalidInput
	}
	if _, err := netip.ParsePrefix(n.CIDR); err != nil {
		return ErrInvalidInput
	}
	return nil
}
