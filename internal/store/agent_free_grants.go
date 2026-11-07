package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// AgentFreeGrantGroupPrefix is the system-owned marker prefix inside
	// public.users.groups used to persist temporary Free grants on bound Custom
	// nodes with zero SQL schema changes. Generic group edit APIs cannot add,
	// modify, or remove markers with this prefix.
	AgentFreeGrantGroupPrefix = "agent:free-grant:"

	AgentFreeGrantStatusActive  = "active"
	AgentFreeGrantStatusRevoked = "revoked"

	DefaultAgentFreeGrantTTL = 72 * time.Hour
	MaxAgentFreeGrantTTL     = 120 * time.Hour

	AuditActionAgentFreeGrantCreate = "agent.free_grant.create"
	AuditActionAgentFreeGrantRenew  = "agent.free_grant.renew"
	AuditActionAgentFreeGrantRevoke = "agent.free_grant.revoke"
)

var (
	ErrAgentFreeGrantNotFound = errors.New("temporary free grant not found")
	ErrAgentFreeGrantInvalid  = errors.New("invalid temporary free grant")
)

type AgentFreeGrant struct {
	NodeID    string     `json:"nodeId"`
	AccountID string     `json:"accountId"`
	Email     string     `json:"email,omitempty"`
	Status    string     `json:"status"`
	ExpiresAt time.Time  `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	ActorID   string     `json:"actorId,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type AgentFreeGrantStore interface {
	ListAgentFreeGrants(ctx context.Context, nodeID string) ([]AgentFreeGrant, error)
	GetAgentFreeGrant(ctx context.Context, nodeID, accountID string) (AgentFreeGrant, bool, error)
	PutAgentFreeGrant(ctx context.Context, grant AgentFreeGrant, audit *AuditLog) (AgentFreeGrant, error)
	RevokeAgentFreeGrant(ctx context.Context, nodeID, accountID, actorID, reason string, now time.Time) (AgentFreeGrant, error)
}

func ValidAgentFreeGrantNodeID(nodeID string) bool {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || nodeID == "*" || len(nodeID) > 128 {
		return false
	}
	if strings.ContainsAny(nodeID, ": /\t\r\n") {
		return false
	}
	return true
}

func normalizeAgentFreeGrant(grant AgentFreeGrant) (AgentFreeGrant, error) {
	grant.NodeID = strings.TrimSpace(grant.NodeID)
	grant.AccountID = strings.TrimSpace(grant.AccountID)
	grant.Email = strings.ToLower(strings.TrimSpace(grant.Email))
	grant.Status = strings.ToLower(strings.TrimSpace(grant.Status))
	grant.ActorID = strings.TrimSpace(grant.ActorID)
	grant.Reason = strings.TrimSpace(grant.Reason)

	if !ValidAgentFreeGrantNodeID(grant.NodeID) || grant.AccountID == "" {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}
	if grant.Status != AgentFreeGrantStatusActive && grant.Status != AgentFreeGrantStatusRevoked {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}
	if grant.ExpiresAt.IsZero() || grant.CreatedAt.IsZero() || grant.UpdatedAt.IsZero() {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}
	grant.ExpiresAt = grant.ExpiresAt.UTC().Truncate(time.Millisecond)
	grant.CreatedAt = grant.CreatedAt.UTC().Truncate(time.Millisecond)
	grant.UpdatedAt = grant.UpdatedAt.UTC().Truncate(time.Millisecond)
	if grant.RevokedAt != nil {
		revoked := grant.RevokedAt.UTC().Truncate(time.Millisecond)
		grant.RevokedAt = &revoked
	}
	if grant.Status == AgentFreeGrantStatusActive {
		if !grant.ExpiresAt.After(grant.UpdatedAt) {
			return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
		}
		if grant.ExpiresAt.Sub(grant.UpdatedAt) > MaxAgentFreeGrantTTL+time.Minute {
			return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
		}
	}
	return grant, nil
}

func EncodeAgentFreeGrantGroup(grant AgentFreeGrant) (string, error) {
	normalized, err := normalizeAgentFreeGrant(grant)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return AgentFreeGrantGroupPrefix + normalized.NodeID + ":" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// ParseAgentFreeGrantGroup decodes a persisted grant marker. Any corrupted or
// mismatched marker fails closed.
func ParseAgentFreeGrantGroup(raw, accountID, email string) (AgentFreeGrant, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, AgentFreeGrantGroupPrefix) {
		return AgentFreeGrant{}, false
	}
	rest := strings.TrimPrefix(raw, AgentFreeGrantGroupPrefix)
	nodeID, encoded, ok := strings.Cut(rest, ":")
	if !ok || !ValidAgentFreeGrantNodeID(nodeID) || encoded == "" {
		return AgentFreeGrant{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return AgentFreeGrant{}, false
	}
	var grant AgentFreeGrant
	if err := json.Unmarshal(decoded, &grant); err != nil {
		return AgentFreeGrant{}, false
	}
	if grant.NodeID != nodeID {
		return AgentFreeGrant{}, false
	}
	accountID = strings.TrimSpace(accountID)
	if accountID != "" && grant.AccountID != "" && grant.AccountID != accountID {
		return AgentFreeGrant{}, false
	}
	if accountID != "" && grant.AccountID == "" {
		grant.AccountID = accountID
	}
	if email != "" {
		grant.Email = strings.ToLower(strings.TrimSpace(email))
	}
	grant.NodeID = strings.TrimSpace(grant.NodeID)
	grant.AccountID = strings.TrimSpace(grant.AccountID)
	grant.Status = strings.ToLower(strings.TrimSpace(grant.Status))
	if !ValidAgentFreeGrantNodeID(grant.NodeID) || grant.AccountID == "" {
		return AgentFreeGrant{}, false
	}
	if grant.Status != AgentFreeGrantStatusActive && grant.Status != AgentFreeGrantStatusRevoked {
		return AgentFreeGrant{}, false
	}
	if grant.ExpiresAt.IsZero() || grant.CreatedAt.IsZero() || grant.UpdatedAt.IsZero() {
		return AgentFreeGrant{}, false
	}
	grant.ExpiresAt = grant.ExpiresAt.UTC()
	grant.CreatedAt = grant.CreatedAt.UTC()
	grant.UpdatedAt = grant.UpdatedAt.UTC()
	if grant.RevokedAt != nil {
		revoked := grant.RevokedAt.UTC()
		grant.RevokedAt = &revoked
	}
	// Enforce the 5-day ceiling even at read time so tampered records fail closed.
	if grant.ExpiresAt.Sub(grant.UpdatedAt) > MaxAgentFreeGrantTTL+time.Minute {
		return AgentFreeGrant{}, false
	}
	return grant, true
}

func ListUserAgentFreeGrants(user *User) []AgentFreeGrant {
	if user == nil || len(user.Groups) == 0 {
		return nil
	}
	byNode := make(map[string]AgentFreeGrant)
	order := make([]string, 0)
	for _, g := range user.Groups {
		if !strings.HasPrefix(strings.TrimSpace(g), AgentFreeGrantGroupPrefix) {
			continue
		}
		grant, ok := ParseAgentFreeGrantGroup(g, user.ID, user.Email)
		if !ok {
			continue
		}
		if _, exists := byNode[grant.NodeID]; !exists {
			order = append(order, grant.NodeID)
		}
		byNode[grant.NodeID] = grant
	}
	out := make([]AgentFreeGrant, 0, len(order))
	for _, nodeID := range order {
		out = append(out, byNode[nodeID])
	}
	return out
}

func ActiveAgentFreeGrant(user *User, nodeID string, now time.Time) (AgentFreeGrant, bool) {
	if user == nil || !user.Active || user.ArchivedAt != nil || !ValidAgentFreeGrantNodeID(nodeID) {
		return AgentFreeGrant{}, false
	}
	now = now.UTC()
	var matched AgentFreeGrant
	found := false
	for _, g := range user.Groups {
		trimmed := strings.TrimSpace(g)
		if !strings.HasPrefix(trimmed, AgentFreeGrantGroupPrefix+nodeID+":") {
			continue
		}
		grant, ok := ParseAgentFreeGrantGroup(trimmed, user.ID, user.Email)
		if !ok {
			return AgentFreeGrant{}, false
		}
		matched = grant
		found = true
	}
	if !found || matched.Status != AgentFreeGrantStatusActive || !now.Before(matched.ExpiresAt) {
		return AgentFreeGrant{}, false
	}
	return matched, true
}

func HasActiveAgentFreeGrant(user *User, nodeID string, now time.Time) bool {
	_, ok := ActiveAgentFreeGrant(user, nodeID, now)
	return ok
}

func WithoutAgentFreeGrantGroups(groups []string) []string {
	if len(groups) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if !strings.HasPrefix(strings.TrimSpace(g), AgentFreeGrantGroupPrefix) {
			out = append(out, g)
		}
	}
	return out
}

func PreserveAgentFreeGrantGroups(existing, requested []string) []string {
	out := make([]string, 0, len(requested)+len(existing))
	for _, g := range requested {
		if !strings.HasPrefix(strings.TrimSpace(g), AgentFreeGrantGroupPrefix) {
			out = append(out, g)
		}
	}
	for _, g := range existing {
		trimmed := strings.TrimSpace(g)
		if strings.HasPrefix(trimmed, AgentFreeGrantGroupPrefix) {
			out = append(out, trimmed)
		}
	}
	return out
}

func replaceUserAgentFreeGrantGroup(groups []string, accountID, email string, grant AgentFreeGrant) ([]string, *AgentFreeGrant, error) {
	encoded, err := EncodeAgentFreeGrantGroup(grant)
	if err != nil {
		return nil, nil, err
	}
	prefix := AgentFreeGrantGroupPrefix + grant.NodeID + ":"
	out := make([]string, 0, len(groups)+1)
	var previous *AgentFreeGrant
	replaced := false
	for _, g := range groups {
		trimmed := strings.TrimSpace(g)
		if strings.HasPrefix(trimmed, prefix) {
			if parsed, ok := ParseAgentFreeGrantGroup(trimmed, accountID, email); ok {
				copyPrev := parsed
				previous = &copyPrev
			}
			if !replaced {
				out = append(out, encoded)
				replaced = true
			}
			continue
		}
		out = append(out, g)
	}
	if !replaced {
		out = append(out, encoded)
	}
	return out, previous, nil
}

func sortAgentFreeGrants(grants []AgentFreeGrant) {
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].NodeID != grants[j].NodeID {
			return grants[i].NodeID < grants[j].NodeID
		}
		if !grants[i].UpdatedAt.Equal(grants[j].UpdatedAt) {
			return grants[i].UpdatedAt.After(grants[j].UpdatedAt)
		}
		return grants[i].AccountID < grants[j].AccountID
	})
}

func (s *memoryStore) ListAgentFreeGrants(ctx context.Context, nodeID string) ([]AgentFreeGrant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nodeID = strings.TrimSpace(nodeID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]AgentFreeGrant, 0)
	for _, u := range s.byID {
		for _, grant := range ListUserAgentFreeGrants(u) {
			if nodeID != "" && grant.NodeID != nodeID {
				continue
			}
			result = append(result, grant)
		}
	}
	sortAgentFreeGrants(result)
	return result, nil
}

func (s *memoryStore) GetAgentFreeGrant(ctx context.Context, nodeID, accountID string) (AgentFreeGrant, bool, error) {
	if err := ctx.Err(); err != nil {
		return AgentFreeGrant{}, false, err
	}
	nodeID = strings.TrimSpace(nodeID)
	accountID = strings.TrimSpace(accountID)
	if !ValidAgentFreeGrantNodeID(nodeID) || accountID == "" {
		return AgentFreeGrant{}, false, ErrAgentFreeGrantInvalid
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.byID[accountID]
	if !ok {
		return AgentFreeGrant{}, false, ErrUserNotFound
	}
	for _, grant := range ListUserAgentFreeGrants(user) {
		if grant.NodeID == nodeID {
			return grant, true, nil
		}
	}
	return AgentFreeGrant{}, false, nil
}

func (s *memoryStore) PutAgentFreeGrant(ctx context.Context, grant AgentFreeGrant, audit *AuditLog) (AgentFreeGrant, error) {
	if err := ctx.Err(); err != nil {
		return AgentFreeGrant{}, err
	}
	if audit == nil || strings.TrimSpace(audit.ActorUUID) == "" {
		return AgentFreeGrant{}, errors.New("temporary free grant audit is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.byID[strings.TrimSpace(grant.AccountID)]
	if !ok {
		return AgentFreeGrant{}, ErrUserNotFound
	}
	if !user.Active || user.ArchivedAt != nil {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}
	now := grant.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC().Truncate(time.Millisecond)
	grant.UpdatedAt = now
	grant.Email = strings.ToLower(strings.TrimSpace(user.Email))
	if grant.Status == "" {
		grant.Status = AgentFreeGrantStatusActive
	}
	for _, existing := range ListUserAgentFreeGrants(user) {
		if existing.NodeID == strings.TrimSpace(grant.NodeID) {
			if grant.CreatedAt.IsZero() {
				grant.CreatedAt = existing.CreatedAt
			}
			break
		}
	}
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = now
	}
	normalized, err := normalizeAgentFreeGrant(grant)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	nextGroups, previous, err := replaceUserAgentFreeGrantGroup(user.Groups, user.ID, user.Email, normalized)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	user.Groups = nextGroups
	user.UpdatedAt = now

	auditCopy := *audit
	if auditCopy.UUID == "" {
		auditCopy.UUID = uuid.NewString()
	}
	if auditCopy.CreatedAt.IsZero() {
		auditCopy.CreatedAt = now
	}
	if auditCopy.Details == nil {
		auditCopy.Details = make(map[string]any)
	}
	auditCopy.Details["target_uuid"] = user.ID
	auditCopy.Details["node_id"] = normalized.NodeID
	auditCopy.Details["before"] = previous
	auditCopy.Details["after"] = normalized
	s.auditLogs = append(s.auditLogs, &auditCopy)
	return normalized, nil
}

func (s *memoryStore) RevokeAgentFreeGrant(ctx context.Context, nodeID, accountID, actorID, reason string, now time.Time) (AgentFreeGrant, error) {
	if err := ctx.Err(); err != nil {
		return AgentFreeGrant{}, err
	}
	nodeID = strings.TrimSpace(nodeID)
	accountID = strings.TrimSpace(accountID)
	actorID = strings.TrimSpace(actorID)
	reason = strings.TrimSpace(reason)
	if !ValidAgentFreeGrantNodeID(nodeID) || accountID == "" || actorID == "" {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC().Truncate(time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.byID[accountID]
	if !ok {
		return AgentFreeGrant{}, ErrUserNotFound
	}
	var current *AgentFreeGrant
	for _, g := range ListUserAgentFreeGrants(user) {
		if g.NodeID == nodeID {
			copyGrant := g
			current = &copyGrant
			break
		}
	}
	if current == nil {
		return AgentFreeGrant{}, ErrAgentFreeGrantNotFound
	}
	revoked := *current
	revoked.Status = AgentFreeGrantStatusRevoked
	revoked.UpdatedAt = now
	revoked.RevokedAt = &now
	revoked.ActorID = actorID
	if reason != "" {
		revoked.Reason = reason
	}
	nextGroups, previous, err := replaceUserAgentFreeGrantGroup(user.Groups, user.ID, user.Email, revoked)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	user.Groups = nextGroups
	user.UpdatedAt = now
	s.auditLogs = append(s.auditLogs, &AuditLog{
		UUID:      uuid.NewString(),
		Action:    AuditActionAgentFreeGrantRevoke,
		ActorUUID: actorID,
		CreatedAt: now,
		Details: map[string]any{
			"target_uuid": user.ID,
			"node_id":     nodeID,
			"reason":      reason,
			"before":      previous,
			"after":       revoked,
		},
	})
	return revoked, nil
}
