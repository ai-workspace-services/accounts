package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

var ErrTeamAgentNotFound = errors.New("team agent enrollment not found")
var ErrTeamAgentConflict = errors.New("team agent enrollment changed")

type TeamAgentEnrollment struct {
	AgentID         string    `json:"agentId"`
	SuggestedTeamID string    `json:"suggestedTeamId"`
	AssignedTeamID  string    `json:"assignedTeamId"`
	Status          string    `json:"status"`
	DecidedBy       string    `json:"decidedBy,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type TeamAgentEnrollmentStore interface {
	RegisterTeamAgent(context.Context, string, string) (TeamAgentEnrollment, error)
	ListTeamAgents(context.Context) ([]TeamAgentEnrollment, error)
	DecideTeamAgent(context.Context, TeamAgentEnrollment, time.Time, *AuditLog) error
}

func (s *memoryStore) RegisterTeamAgent(ctx context.Context, id, suggested string) (TeamAgentEnrollment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.teamAgentEnrollments == nil {
		s.teamAgentEnrollments = map[string]TeamAgentEnrollment{}
	}
	e, ok := s.teamAgentEnrollments[id]
	if !ok {
		e = TeamAgentEnrollment{AgentID: id, SuggestedTeamID: suggested, Status: "pending", UpdatedAt: time.Now().UTC()}
		s.teamAgentEnrollments[id] = e
	}
	return e, nil
}
func (s *memoryStore) ListTeamAgents(ctx context.Context) ([]TeamAgentEnrollment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TeamAgentEnrollment, 0, len(s.teamAgentEnrollments))
	for _, e := range s.teamAgentEnrollments {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}
func (s *memoryStore) DecideTeamAgent(ctx context.Context, e TeamAgentEnrollment, expected time.Time, audit *AuditLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.teamAgentEnrollments[e.AgentID]
	if !ok {
		return ErrTeamAgentNotFound
	}
	if !old.UpdatedAt.Equal(expected) {
		return ErrTeamAgentConflict
	}
	e.UpdatedAt = time.Now().UTC()
	if !e.UpdatedAt.After(old.UpdatedAt) {
		e.UpdatedAt = old.UpdatedAt.Add(time.Microsecond)
	}
	s.teamAgentEnrollments[e.AgentID] = e
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return nil
}
func scanTeamAgent(row interface{ Scan(...any) error }) (TeamAgentEnrollment, error) {
	var e TeamAgentEnrollment
	err := row.Scan(&e.AgentID, &e.SuggestedTeamID, &e.AssignedTeamID, &e.Status, &e.DecidedBy, &e.Reason, &e.UpdatedAt)
	return e, err
}

const teamAgentColumns = "agent_id,suggested_team_id,assigned_team_id,status,decided_by,reason,updated_at"

func (s *postgresStore) RegisterTeamAgent(ctx context.Context, id, suggested string) (TeamAgentEnrollment, error) {
	// Never let telemetry overwrite an administrator decision or its version.
	_, err := s.db.ExecContext(ctx, `INSERT INTO team_agent_enrollments(agent_id,suggested_team_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, suggested)
	if err != nil {
		return TeamAgentEnrollment{}, err
	}
	return scanTeamAgent(s.db.QueryRowContext(ctx, `SELECT `+teamAgentColumns+` FROM team_agent_enrollments WHERE agent_id=$1`, id))
}
func (s *postgresStore) ListTeamAgents(ctx context.Context) ([]TeamAgentEnrollment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+teamAgentColumns+` FROM team_agent_enrollments ORDER BY agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamAgentEnrollment{}
	for rows.Next() {
		e, err := scanTeamAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *postgresStore) DecideTeamAgent(ctx context.Context, e TeamAgentEnrollment, expected time.Time, audit *AuditLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE team_agent_enrollments SET assigned_team_id=$2,status=$3,decided_by=$4,reason=$5,updated_at=GREATEST(clock_timestamp(),updated_at + interval '1 microsecond') WHERE agent_id=$1 AND updated_at=$6`, e.AgentID, e.AssignedTeamID, e.Status, e.DecidedBy, e.Reason, expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTeamAgentConflict
	}
	details, err := json.Marshal(audit.Details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs (action,actor_uuid,details) VALUES($1,$2,$3)`, audit.Action, audit.ActorUUID, details)
	if err != nil {
		return err
	}
	return tx.Commit()
}
