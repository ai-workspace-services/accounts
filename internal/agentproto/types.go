package agentproto

import (
	"time"

	"account/internal/xrayconfig"
)

// ClientListResponse represents the payload returned by the controller when an
// agent requests the latest set of Xray clients.
type ClientListResponse struct {
	AgentID          string              `json:"agentId,omitempty"`
	NodeID           string              `json:"nodeId,omitempty"`
	EnrollmentStatus string              `json:"enrollmentStatus,omitempty"`
	Scope            *UserScope          `json:"scope,omitempty"`
	RoutingPolicy    *RoutingPolicy      `json:"routingPolicy,omitempty"`
	Clients          []xrayconfig.Client `json:"clients"`
	Total            int                 `json:"total"`
	GeneratedAt      time.Time           `json:"generatedAt"`
	Revision         string              `json:"revision,omitempty"`
}

// RoutingPolicy conveys the controller's authoritative pool and three-tier
// traffic-split policy to an edge agent for its authenticated scope.
type RoutingPolicy struct {
	Version             int      `json:"version"`
	Revision            string   `json:"revision"`
	Pool                string   `json:"pool"`
	PublicResource      bool     `json:"publicResource"`
	MediaOnProAction    string   `json:"mediaOnProAction"`
	FallbackDefault     bool     `json:"fallbackDefault,omitempty"`
	Unlimited           bool     `json:"unlimited,omitempty"`
	PricingGroups       []string `json:"pricingGroups,omitempty"`
	SubPoolTags         []string `json:"subPoolTags,omitempty"`
	DirectDomains       []string `json:"directDomains,omitempty"`
	DirectIPs           []string `json:"directIps,omitempty"`
	MediaDomains        []string `json:"mediaDomains,omitempty"`
	ProWhitelistDomains []string `json:"proWhitelistDomains,omitempty"`
}

// UserScope is derived from an enrolled credential or an administrator-approved
// node assignment. Node-supplied team suggestions never grant authorization.
type UserScope struct {
	EmailPolicy     string `json:"emailPolicy,omitempty"`
	EmailDigest     string `json:"emailDigest,omitempty"`
	CustomAllowFree bool   `json:"customAllowFree,omitempty"`
	ResourceClass   string `json:"resourceClass"`
	TeamID          string `json:"teamId,omitempty"`
}

// StatusReport captures the runtime state of an agent and the managed Xray
// instance.
type StatusReport struct {
	NodeTraffic      *NodeTrafficSample `json:"nodeTraffic,omitempty"`
	Overlay          *OverlayStatus     `json:"overlay,omitempty"`
	GatewayTelemetry *GatewayTelemetry  `json:"gatewayTelemetry,omitempty"`
	AgentID          string             `json:"agentId"` // Self-reported agent ID (e.g., "hk-xhttp.svc.plus")
	Role             string             `json:"role,omitempty"`
	Healthy          bool               `json:"healthy"`
	Message          string             `json:"message,omitempty"`
	Users            int                `json:"users"`
	SyncRevision     string             `json:"syncRevision,omitempty"`
	Xray             XrayStatus         `json:"xray"`
}

type NodeTrafficSample struct {
	Interface string    `json:"interface"`
	BootID    string    `json:"bootId"`
	RXBytes   *int64    `json:"rxBytes"`
	TXBytes   *int64    `json:"txBytes"`
	SampledAt time.Time `json:"sampledAt"`
}

// XrayStatus describes the synchronisation state of the managed Xray process.
type XrayStatus struct {
	Provider        string     `json:"provider,omitempty"`
	Product         string     `json:"product,omitempty"`
	CustomAllowFree bool       `json:"customAllowFree,omitempty"`
	AllowedEmails   []string   `json:"allowedEmails,omitempty"`
	Running         bool       `json:"running"`
	Clients         int        `json:"clients"`
	LastSync        *time.Time `json:"lastSync,omitempty"`
	ConfigHash      string     `json:"configHash,omitempty"`
	NodeID          string     `json:"nodeId,omitempty"`
	NetworkID       string     `json:"networkId,omitempty"`
	Region          string     `json:"region,omitempty"`
	Pool            string     `json:"pool,omitempty"`
	EntryPoint      string     `json:"entryPoint,omitempty"`
	OpenToUsers     *bool      `json:"openToUsers,omitempty"`
	LineCode        string     `json:"lineCode,omitempty"`
	PricingGroup    string     `json:"pricingGroup,omitempty"`
	RoutingPool     string     `json:"routingPool,omitempty"`
	SubPoolTags     []string   `json:"subPoolTags,omitempty"`
	StatsEnabled    bool       `json:"statsEnabled"`
	XrayRevision    string     `json:"xrayRevision,omitempty"`
}

// OverlayStatus is non-secret telemetry, never enrollment or routing authority.
type OverlayStatus struct {
	NetworkID    string        `json:"network_id"`
	DeviceID     string        `json:"device_id"`
	Role         string        `json:"role"`
	Capabilities []string      `json:"capabilities"`
	Healthy      bool          `json:"healthy"`
	UpdatedAt    time.Time     `json:"updated_at"`
	ExpiresAt    time.Time     `json:"expires_at"`
	Paths        []OverlayPath `json:"paths,omitempty"`
}
type OverlayPath struct {
	SentPackets     uint64 `json:"sent_packets"`
	ReceivedPackets uint64 `json:"received_packets"`
	DeviceID        string `json:"device_id"`
	Path            string `json:"path"`
	Endpoint        string `json:"endpoint,omitempty"`
	RTTMillis       int64  `json:"rtt_ms,omitempty"`
	Reason          string `json:"reason"`
}

type GatewayTelemetry struct {
	SchemaVersion       int                    `json:"schema_version"`
	NetworkID           string                 `json:"network_id"`
	GatewayID           string                 `json:"gateway_id"`
	EndpointHost        string                 `json:"endpoint_host,omitempty"`
	ConnectionMode      string                 `json:"connection_mode,omitempty"`
	AppliedConfigID     string                 `json:"applied_config_id,omitempty"`
	AppliedGeneration   uint64                 `json:"applied_generation,omitempty"`
	WireGuardRunning    bool                   `json:"wireguard_running"`
	XrayRunning         bool                   `json:"xray_running"`
	RelayRunning        bool                   `json:"relay_running"`
	RelayActiveSessions int                    `json:"relay_active_sessions,omitempty"`
	SampledAt           time.Time              `json:"sampled_at"`
	ExpiresAt           time.Time              `json:"expires_at"`
	Traffic             GatewayTrafficSummary  `json:"traffic"`
	Peers               []GatewayPeerTelemetry `json:"peers,omitempty"`
	TopApps             []GatewayAppTraffic    `json:"top_apps,omitempty"`
}

type GatewayTrafficSummary struct {
	RxBytesTotal    uint64 `json:"rx_bytes_total"`
	TxBytesTotal    uint64 `json:"tx_bytes_total"`
	InternetRxBytes uint64 `json:"internet_rx_bytes"`
	InternetTxBytes uint64 `json:"internet_tx_bytes"`
	PrivateRxBytes  uint64 `json:"private_rx_bytes"`
	PrivateTxBytes  uint64 `json:"private_tx_bytes"`
}

type GatewayPeerTelemetry struct {
	DeviceID          string     `json:"device_id"`
	WireGuardAddress  string     `json:"wireguard_address,omitempty"`
	Connected         bool       `json:"connected"`
	Path              string     `json:"path,omitempty"`
	Endpoint          string     `json:"endpoint,omitempty"`
	LatestHandshakeAt *time.Time `json:"latest_handshake_at,omitempty"`
	RxBytes           uint64     `json:"rx_bytes"`
	TxBytes           uint64     `json:"tx_bytes"`
	RTTMillis         int64      `json:"rtt_ms,omitempty"`
}

type GatewayAppTraffic struct {
	App         string `json:"app"`
	RxBytes     uint64 `json:"rx_bytes"`
	TxBytes     uint64 `json:"tx_bytes"`
	Connections int    `json:"connections"`
}
