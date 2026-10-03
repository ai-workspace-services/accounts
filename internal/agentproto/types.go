package agentproto

import (
	"time"

	"account/internal/xrayconfig"
)

// ClientListResponse represents the payload returned by the controller when an
// agent requests the latest set of Xray clients.
type ClientListResponse struct {
	Clients     []xrayconfig.Client `json:"clients"`
	Total       int                 `json:"total"`
	GeneratedAt time.Time           `json:"generatedAt"`
	Revision    string              `json:"revision,omitempty"`
}

// StatusReport captures the runtime state of an agent and the managed Xray
// instance.
type StatusReport struct {
	Overlay      *OverlayStatus `json:"overlay,omitempty"`
	AgentID      string         `json:"agentId"` // Self-reported agent ID (e.g., "hk-xhttp.svc.plus")
	Role         string         `json:"role,omitempty"`
	Healthy      bool           `json:"healthy"`
	Message      string         `json:"message,omitempty"`
	Users        int            `json:"users"`
	SyncRevision string         `json:"syncRevision,omitempty"`
	Xray         XrayStatus     `json:"xray"`
}

// XrayStatus describes the synchronisation state of the managed Xray process.
type XrayStatus struct {
	Running      bool       `json:"running"`
	Clients      int        `json:"clients"`
	LastSync     *time.Time `json:"lastSync,omitempty"`
	ConfigHash   string     `json:"configHash,omitempty"`
	NodeID       string     `json:"nodeId,omitempty"`
	NetworkID    string     `json:"networkId,omitempty"`
	Region       string     `json:"region,omitempty"`
	Pool         string     `json:"pool,omitempty"`
	EntryPoint   string     `json:"entryPoint,omitempty"`
	OpenToUsers  *bool      `json:"openToUsers,omitempty"`
	LineCode     string     `json:"lineCode,omitempty"`
	PricingGroup string     `json:"pricingGroup,omitempty"`
	StatsEnabled bool       `json:"statsEnabled"`
	XrayRevision string     `json:"xrayRevision,omitempty"`
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
