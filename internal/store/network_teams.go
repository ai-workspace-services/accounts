package store

import (
	"context"
	"errors"
	"sort"
	"time"
)

var ErrNetworkTeamNotFound = errors.New("network team not found")
var ErrNetworkWithdrawalRequired = errors.New("withdraw existing node authorization before joining an active team")
var ErrNetworkTeamFull = errors.New("network team member limit reached")

type NetworkTeam struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MemberLimit int       `json:"memberLimit"`
	ValidFrom   time.Time `json:"validFrom"`
	ValidUntil  time.Time `json:"validUntil"`
	Active      bool      `json:"active"`
}

type NetworkMembership struct {
	Configured    bool   `json:"-"`
	UserID        string `json:"userId"`
	TeamID        string `json:"teamId"`
	DedicatedOnly bool   `json:"dedicatedOnly"`
}

// NetworkTeamStore is additive: old accounts have no membership and remain personal.
// An unbound dedicated membership is retained after removal; only an explicit
// administrator change to personal restores public-node access.
type NetworkTeamStore interface {
	ListNetworkTeams(context.Context) ([]NetworkTeam, error)
	ListNetworkMemberships(context.Context) ([]NetworkMembership, error)
	PutNetworkTeam(context.Context, NetworkTeam, *AuditLog) error
	GetNetworkMembership(context.Context, string) (NetworkMembership, error)
	PutNetworkMembership(context.Context, NetworkMembership, *AuditLog) error
}

func (s *memoryStore) ListNetworkTeams(ctx context.Context) ([]NetworkTeam, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]NetworkTeam, 0, len(s.networkTeams))
	for _, team := range s.networkTeams {
		result = append(result, team)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *memoryStore) PutNetworkTeam(ctx context.Context, team NetworkTeam, audit *AuditLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, member := range s.networkMembers {
		if member.TeamID == team.ID {
			count++
		}
	}
	if count > team.MemberLimit {
		return ErrNetworkTeamFull
	}
	if s.networkTeams == nil {
		s.networkTeams = map[string]NetworkTeam{}
	}
	s.networkTeams[team.ID] = team
	s.auditLogs = append(s.auditLogs, audit)
	return nil
}

func (s *memoryStore) GetNetworkMembership(ctx context.Context, userID string) (NetworkMembership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	member, ok := s.networkMembers[userID]
	member.Configured = ok
	if !ok {
		member.UserID = userID
	}
	return member, nil
}

func (s *memoryStore) PutNetworkMembership(ctx context.Context, member NetworkMembership, audit *AuditLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[member.UserID] == nil {
		return ErrUserNotFound
	}
	if member.TeamID != "" {
		team, ok := s.networkTeams[member.TeamID]
		if !ok {
			return ErrNetworkTeamNotFound
		}
		current := s.networkMembers[member.UserID]
		if team.Active && (!current.DedicatedOnly || (current.TeamID != "" && current.TeamID != member.TeamID)) {
			return ErrNetworkWithdrawalRequired
		}
		count := 0
		for id, other := range s.networkMembers {
			if id != member.UserID && other.TeamID == member.TeamID {
				count++
			}
		}
		if count >= team.MemberLimit {
			return ErrNetworkTeamFull
		}
		member.DedicatedOnly = true
	}
	if s.networkMembers == nil {
		s.networkMembers = map[string]NetworkMembership{}
	}
	s.networkMembers[member.UserID] = member
	s.auditLogs = append(s.auditLogs, audit)
	return nil
}

func (s *memoryStore) ListNetworkMemberships(ctx context.Context) ([]NetworkMembership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]NetworkMembership, 0, len(s.networkMembers))
	for _, member := range s.networkMembers {
		result = append(result, member)
	}
	return result, nil
}
