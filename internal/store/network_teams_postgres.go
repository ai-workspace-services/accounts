package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

func (s *postgresStore) ListNetworkTeams(ctx context.Context) ([]NetworkTeam, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,member_limit,valid_from,valid_until,active FROM network_teams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]NetworkTeam, 0)
	for rows.Next() {
		var team NetworkTeam
		if err := rows.Scan(&team.ID, &team.Name, &team.MemberLimit, &team.ValidFrom, &team.ValidUntil, &team.Active); err != nil {
			return nil, err
		}
		result = append(result, team)
	}
	return result, rows.Err()
}

func (s *postgresStore) PutNetworkTeam(ctx context.Context, team NetworkTeam, audit *AuditLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize cap changes and membership moves across all Accounts instances.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(2026101201)`); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM network_team_members WHERE team_id=$1`, team.ID).Scan(&count); err != nil {
		return err
	}
	if count > team.MemberLimit {
		return ErrNetworkTeamFull
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO network_teams(id,name,member_limit,valid_from,valid_until,active) VALUES($1,$2,$3,$4,$5,$6)
	 ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,member_limit=EXCLUDED.member_limit,valid_from=EXCLUDED.valid_from,valid_until=EXCLUDED.valid_until,active=EXCLUDED.active`, team.ID, team.Name, team.MemberLimit, team.ValidFrom, team.ValidUntil, team.Active)
	if err != nil {
		return err
	}
	if err = insertNetworkAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetNetworkMembership(ctx context.Context, userID string) (NetworkMembership, error) {
	member := NetworkMembership{UserID: userID}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(team_id,''),dedicated_only FROM network_team_members WHERE user_uuid=$1`, userID).Scan(&member.TeamID, &member.DedicatedOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return member, nil
	}
	member.Configured = err == nil
	return member, err
}

func (s *postgresStore) PutNetworkMembership(ctx context.Context, member NetworkMembership, audit *AuditLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(2026101201)`); err != nil {
		return err
	}
	if member.TeamID != "" {
		var limit, count int
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT member_limit,active FROM network_teams WHERE id=$1`, member.TeamID).Scan(&limit, &active)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNetworkTeamNotFound
		}
		if err != nil {
			return err
		}
		var oldTeam string
		var oldDedicated bool
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(team_id,''),dedicated_only FROM network_team_members WHERE user_uuid=$1`, member.UserID).Scan(&oldTeam, &oldDedicated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if active && (!oldDedicated || (oldTeam != "" && oldTeam != member.TeamID)) {
			return ErrNetworkWithdrawalRequired
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM network_team_members WHERE team_id=$1 AND user_uuid<>$2`, member.TeamID, member.UserID).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return ErrNetworkTeamFull
		}
		member.DedicatedOnly = true
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO network_team_members(user_uuid,team_id,dedicated_only) VALUES($1,NULLIF($2,''),$3)
	 ON CONFLICT(user_uuid) DO UPDATE SET team_id=EXCLUDED.team_id,dedicated_only=EXCLUDED.dedicated_only`, member.UserID, member.TeamID, member.DedicatedOnly)
	if err != nil {
		return err
	}
	if err = insertNetworkAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func insertNetworkAudit(ctx context.Context, tx *sql.Tx, audit *AuditLog) error {
	if audit == nil {
		return errors.New("network access audit is required")
	}
	details, err := json.Marshal(audit.Details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_logs(uuid,action,actor_uuid,details) VALUES($1,$2,NULLIF($3,'')::uuid,$4)`, uuid.NewString(), audit.Action, audit.ActorUUID, details)
	return err
}

func (s *postgresStore) ListNetworkMemberships(ctx context.Context) ([]NetworkMembership, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_uuid::text,COALESCE(team_id,''),dedicated_only FROM network_team_members`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]NetworkMembership, 0)
	for rows.Next() {
		var member NetworkMembership
		if err = rows.Scan(&member.UserID, &member.TeamID, &member.DedicatedOnly); err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}
