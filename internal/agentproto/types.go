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
	AgentID      string     `json:"agentId"` // Self-reported agent ID (e.g., "hk-xhttp.svc.plus")
	Role         string     `json:"role,omitempty"`
	Healthy      bool       `json:"healthy"`
	Message      string     `json:"message,omitempty"`
	Users        int        `json:"users"`
	SyncRevision string     `json:"syncRevision,omitempty"`
	Xray         XrayStatus `json:"xray"`
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
	LineCode     string     `json:"lineCode,omitempty"`
	PricingGroup string     `json:"pricingGroup,omitempty"`
	StatsEnabled bool       `json:"statsEnabled"`
	XrayRevision string     `json:"xrayRevision,omitempty"`
}
