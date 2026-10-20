package agentserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"account/internal/agentproto"
	"account/internal/store"
	"log/slog"
)

// Credential defines the authentication material assigned to a managed agent.
type Credential struct {
	ID     string   `yaml:"id"`
	Name   string   `yaml:"name"`
	Token  string   `yaml:"token"`
	Groups []string `yaml:"groups"`
}

// Config groups the credential set exposed through configuration.
type Config struct {
	Credentials []Credential `yaml:"credentials"`
}

// Identity represents an authenticated agent instance.
type Identity struct {
	ID     string
	Name   string
	Groups []string
}

// StatusSnapshot captures the last reported status for an agent.
type StatusSnapshot struct {
	Agent     Identity
	Report    agentproto.StatusReport
	UpdatedAt time.Time
}

// Registry manages agent credentials and status reports in-memory.
type Registry struct {
	mu            sync.RWMutex
	credentials   map[[32]byte]Identity
	byID          map[string]Identity
	statuses      map[string]StatusSnapshot
	sandboxAgents map[string]bool
	store         store.Store
	logger        *slog.Logger
}

// NewRegistry constructs a registry from configuration, validating credentials
// and normalising their representation.
func NewRegistry(cfg Config) (*Registry, error) {
	r := &Registry{
		credentials:   make(map[[32]byte]Identity),
		byID:          make(map[string]Identity),
		statuses:      make(map[string]StatusSnapshot),
		sandboxAgents: make(map[string]bool),
		logger:        slog.Default().With("component", "agent-registry"),
	}

	for _, cred := range cfg.Credentials {
		id := strings.TrimSpace(cred.ID)
		token := strings.TrimSpace(cred.Token)
		if id == "" {
			return nil, errors.New("agent credential id is required")
		}
		if token == "" {
			return nil, errors.New("agent credential token is required")
		}
		if _, exists := r.byID[id]; exists {
			return nil, errors.New("duplicate agent credential id: " + id)
		}

		digest := sha256.Sum256([]byte(token))
		if _, exists := r.credentials[digest]; exists {
			return nil, errors.New("duplicate agent credential token")
		}

		identity := Identity{
			ID:     id,
			Name:   strings.TrimSpace(cred.Name),
			Groups: normalizeStrings(cred.Groups),
		}
		r.credentials[digest] = identity
		r.byID[id] = identity
	}

	return r, nil
}

// SetStore configures a persistence store for the registry.
func (r *Registry) SetStore(st store.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = st
}

// SetLogger overrides the default logger.
func (r *Registry) SetLogger(logger *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if logger != nil {
		r.logger = logger
	}
}

// Authenticate validates the provided token and returns the associated agent
// identity when successful.
func (r *Registry) Authenticate(token string) (*Identity, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))

	r.mu.RLock()
	identity, ok := r.credentials[digest]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}

	copy := identity
	return &copy, true
}

// ReportStatus records the status report for the provided agent identity.
func (r *Registry) ReportStatus(agent Identity, report agentproto.StatusReport) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Public class reconciliation is authorized by the API before reporting.
	// Keep inventory identity and live status in agreement after a class change.
	r.byID[agent.ID] = agent
	r.statuses[agent.ID] = StatusSnapshot{
		Agent:     agent,
		Report:    report,
		UpdatedAt: time.Now().UTC(),
	}

	// Persist to store if configured
	if r.store != nil {
		go func(a Identity, rep agentproto.StatusReport) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			now := time.Now().UTC()
			dbAgent := &store.Agent{
				ID:            a.ID,
				Name:          a.Name,
				Groups:        a.Groups,
				Healthy:       rep.Healthy,
				LastHeartbeat: &now,
				ClientsCount:  rep.Xray.Clients,
				SyncRevision:  rep.SyncRevision,
			}
			if err := r.store.UpsertAgent(ctx, dbAgent); err != nil {
				r.logger.Error("failed to persist agent status heartbeat", "agent", a.ID, "err", err)
			}
		}(agent, report)
	}
}

// RegisterAgent dynamically registers an agent with the given ID if it doesn't already exist.
// This allows agents to self-report their IDs when using a shared authentication token.
// The agent will inherit the groups from the credential used for authentication.
// Returns the identity for the agent (either existing or newly created).
func (r *Registry) RegisterAgent(agentID string, groups []string) Identity {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if agent already registered
	if identity, exists := r.byID[agentID]; exists {
		return identity
	}

	// Create new identity for this agent
	identity := Identity{
		ID:     agentID,
		Name:   agentID, // Use ID as name by default
		Groups: groups,
	}

	r.byID[agentID] = identity

	// Persist to store if configured
	if r.store != nil {
		go func(id string, g []string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			dbAgent := &store.Agent{
				ID:     id,
				Name:   id,
				Groups: g,
			}
			if err := r.store.UpsertAgent(ctx, dbAgent); err != nil {
				r.logger.Error("failed to persist dynamically registered agent", "agent", id, "err", err)
			}
		}(agentID, groups)
	}

	return identity
}

// Load populates the registry from the persistence store.
func (r *Registry) Load(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	agents, err := r.store.ListAgents(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range agents {
		if _, exists := r.byID[a.ID]; !exists {
			identity := Identity{
				ID:     a.ID,
				Name:   a.Name,
				Groups: a.Groups,
			}
			r.byID[a.ID] = identity
			if a.LastHeartbeat != nil {
				r.statuses[a.ID] = StatusSnapshot{
					Agent: identity,
					Report: agentproto.StatusReport{
						AgentID:      a.ID,
						Healthy:      a.Healthy,
						Users:        a.ClientsCount,
						SyncRevision: a.SyncRevision,
						Xray: agentproto.XrayStatus{
							Clients: a.ClientsCount,
						},
					},
					UpdatedAt: *a.LastHeartbeat,
				}
			}
		}
	}
	return nil
}

// Statuses returns the latest status snapshot for all agents sorted by ID.
func (r *Registry) Statuses() []StatusSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snapshots := make([]StatusSnapshot, 0, len(r.byID))
	for id, identity := range r.byID {
		snapshot, ok := r.statuses[id]
		if !ok {
			snapshot = StatusSnapshot{Agent: identity}
		}
		snapshots = append(snapshots, snapshot)
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Agent.ID < snapshots[j].Agent.ID
	})

	return snapshots
}

// Agents returns the configured agent identities in a deterministic order.
func (r *Registry) Agents() []Identity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agents := make([]Identity, 0, len(r.byID))
	for _, identity := range r.byID {
		agents = append(agents, identity)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].ID < agents[j].ID
	})
	return agents
}

// IsSandboxAgent reports whether the provided agent ID is bound to sandbox mode.
func (r *Registry) IsSandboxAgent(agentID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sandboxAgents[agentID]
}

// SetSandboxAgent marks an agent as a sandbox agent.
func (r *Registry) SetSandboxAgent(agentID string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if enabled {
		r.sandboxAgents[agentID] = true
	} else {
		delete(r.sandboxAgents, agentID)
	}
}

// ClearSandboxAgents clears all sandbox agent bindings.
func (r *Registry) ClearSandboxAgents() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxAgents = make(map[string]bool)
}

// normalizeStrings trims whitespace and removes duplicates from the provided
// slice while preserving the original order for unique entries.
func normalizeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// SetAgentGroups updates inventory atomically after the API resolves persisted
// enrollment authority. It never changes the authentication credential itself.
func (r *Registry) SetAgentGroups(id string, groups []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	identity, ok := r.byID[id]
	if !ok {
		return
	}
	identity.Groups = append([]string(nil), groups...)
	r.byID[id] = identity
	if snapshot, ok := r.statuses[id]; ok {
		snapshot.Agent = identity
		r.statuses[id] = snapshot
	}
}
