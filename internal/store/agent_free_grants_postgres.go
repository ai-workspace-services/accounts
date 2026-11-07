package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *postgresStore) ListAgentFreeGrants(ctx context.Context, nodeID string) ([]AgentFreeGrant, error) {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if !caps.hasGroups {
		return nil, errors.New("temporary free grants require users.groups")
	}
	nodeID = strings.TrimSpace(nodeID)
	pattern := AgentFreeGrantGroupPrefix + "%"
	if nodeID != "" {
		if !ValidAgentFreeGrantNodeID(nodeID) {
			return nil, ErrAgentFreeGrantInvalid
		}
		pattern = AgentFreeGrantGroupPrefix + nodeID + ":%"
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT uuid::text, email, groups
		FROM public.users
		WHERE jsonb_typeof(groups) = 'array'
		  AND EXISTS (
			SELECT 1
			FROM jsonb_array_elements_text(groups) AS g
			WHERE g LIKE $1
		  )
		ORDER BY uuid ASC
	`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]AgentFreeGrant, 0)
	for rows.Next() {
		var accountID, email string
		var groupsRaw []byte
		if err := rows.Scan(&accountID, &email, &groupsRaw); err != nil {
			return nil, err
		}
		user := &User{
			ID:     accountID,
			Email:  email,
			Groups: decodeStringSlice(groupsRaw),
		}
		for _, grant := range ListUserAgentFreeGrants(user) {
			if nodeID != "" && grant.NodeID != nodeID {
				continue
			}
			result = append(result, grant)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortAgentFreeGrants(result)
	return result, nil
}

func (s *postgresStore) GetAgentFreeGrant(ctx context.Context, nodeID, accountID string) (AgentFreeGrant, bool, error) {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return AgentFreeGrant{}, false, err
	}
	if !caps.hasGroups {
		return AgentFreeGrant{}, false, errors.New("temporary free grants require users.groups")
	}
	nodeID = strings.TrimSpace(nodeID)
	accountID = strings.TrimSpace(accountID)
	if !ValidAgentFreeGrantNodeID(nodeID) || accountID == "" {
		return AgentFreeGrant{}, false, ErrAgentFreeGrantInvalid
	}

	var email string
	var groupsRaw []byte
	err = s.db.QueryRowContext(ctx, `SELECT email, groups FROM public.users WHERE uuid = $1`, accountID).Scan(&email, &groupsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentFreeGrant{}, false, ErrUserNotFound
	}
	if err != nil {
		return AgentFreeGrant{}, false, err
	}
	user := &User{
		ID:     accountID,
		Email:  email,
		Groups: decodeStringSlice(groupsRaw),
	}
	for _, grant := range ListUserAgentFreeGrants(user) {
		if grant.NodeID == nodeID {
			return grant, true, nil
		}
	}
	return AgentFreeGrant{}, false, nil
}

func (s *postgresStore) PutAgentFreeGrant(ctx context.Context, grant AgentFreeGrant, audit *AuditLog) (AgentFreeGrant, error) {
	if audit == nil || strings.TrimSpace(audit.ActorUUID) == "" {
		return AgentFreeGrant{}, errors.New("temporary free grant audit is required")
	}
	caps, err := s.capabilities(ctx)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	if !caps.hasGroups {
		return AgentFreeGrant{}, errors.New("temporary free grants require users.groups")
	}

	accountID := strings.TrimSpace(grant.AccountID)
	nodeID := strings.TrimSpace(grant.NodeID)
	if !ValidAgentFreeGrantNodeID(nodeID) || accountID == "" {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	defer tx.Rollback()

	var email string
	var groupsRaw []byte
	var active bool
	var archivedAt sql.NullTime
	archivedExpr := "NULL::timestamptz"
	if caps.hasArchivedAt {
		archivedExpr = "archived_at"
	}
	activeExpr := "true"
	if caps.hasActive {
		activeExpr = "coalesce(active, true)"
	}

	query := "SELECT email, groups, " + activeExpr + ", " + archivedExpr + " FROM public.users WHERE uuid = $1 FOR UPDATE"
	if err := tx.QueryRowContext(ctx, query, accountID).Scan(&email, &groupsRaw, &active, &archivedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentFreeGrant{}, ErrUserNotFound
		}
		return AgentFreeGrant{}, err
	}
	if !active || archivedAt.Valid {
		return AgentFreeGrant{}, ErrAgentFreeGrantInvalid
	}

	currentGroups := decodeStringSlice(groupsRaw)
	now := grant.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC().Truncate(time.Millisecond)
	grant.UpdatedAt = now
	grant.Email = strings.ToLower(strings.TrimSpace(email))
	if grant.Status == "" {
		grant.Status = AgentFreeGrantStatusActive
	}
	for _, existing := range ListUserAgentFreeGrants(&User{ID: accountID, Email: email, Groups: currentGroups}) {
		if existing.NodeID == nodeID {
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
	nextGroups, previous, err := replaceUserAgentFreeGrantGroup(currentGroups, accountID, email, normalized)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	encodedGroups, err := encodeStringSlice(nextGroups)
	if err != nil {
		return AgentFreeGrant{}, err
	}

	if caps.hasUpdatedAt {
		if _, err := tx.ExecContext(ctx, `UPDATE public.users SET groups = $2, updated_at = $3 WHERE uuid = $1`, accountID, encodedGroups, now); err != nil {
			return AgentFreeGrant{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE public.users SET groups = $2 WHERE uuid = $1`, accountID, encodedGroups); err != nil {
			return AgentFreeGrant{}, err
		}
	}

	details := make(map[string]any, len(audit.Details)+4)
	for k, v := range audit.Details {
		details[k] = v
	}
	details["target_uuid"] = accountID
	details["node_id"] = normalized.NodeID
	details["before"] = previous
	details["after"] = normalized
	detailsRaw, err := json.Marshal(details)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	auditID := strings.TrimSpace(audit.UUID)
	if auditID == "" {
		auditID = uuid.NewString()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.audit_logs (uuid, action, actor_uuid, details, created_at)
		VALUES ($1, $2, NULLIF($3,'')::uuid, $4, $5)
	`, auditID, audit.Action, strings.TrimSpace(audit.ActorUUID), detailsRaw, now); err != nil {
		return AgentFreeGrant{}, err
	}

	if err := tx.Commit(); err != nil {
		return AgentFreeGrant{}, err
	}
	return normalized, nil
}

func (s *postgresStore) RevokeAgentFreeGrant(ctx context.Context, nodeID, accountID, actorID, reason string, now time.Time) (AgentFreeGrant, error) {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	if !caps.hasGroups {
		return AgentFreeGrant{}, errors.New("temporary free grants require users.groups")
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	defer tx.Rollback()

	var email string
	var groupsRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT email, groups FROM public.users WHERE uuid = $1 FOR UPDATE`, accountID).Scan(&email, &groupsRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentFreeGrant{}, ErrUserNotFound
		}
		return AgentFreeGrant{}, err
	}
	currentGroups := decodeStringSlice(groupsRaw)
	var current *AgentFreeGrant
	for _, existing := range ListUserAgentFreeGrants(&User{ID: accountID, Email: email, Groups: currentGroups}) {
		if existing.NodeID == nodeID {
			copyGrant := existing
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
	nextGroups, previous, err := replaceUserAgentFreeGrantGroup(currentGroups, accountID, email, revoked)
	if err != nil {
		return AgentFreeGrant{}, err
	}
	encodedGroups, err := encodeStringSlice(nextGroups)
	if err != nil {
		return AgentFreeGrant{}, err
	}

	if caps.hasUpdatedAt {
		if _, err := tx.ExecContext(ctx, `UPDATE public.users SET groups = $2, updated_at = $3 WHERE uuid = $1`, accountID, encodedGroups, now); err != nil {
			return AgentFreeGrant{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE public.users SET groups = $2 WHERE uuid = $1`, accountID, encodedGroups); err != nil {
			return AgentFreeGrant{}, err
		}
	}

	detailsRaw, err := json.Marshal(map[string]any{
		"target_uuid": accountID,
		"node_id":     nodeID,
		"reason":      reason,
		"before":      previous,
		"after":       revoked,
	})
	if err != nil {
		return AgentFreeGrant{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.audit_logs (uuid, action, actor_uuid, details, created_at)
		VALUES ($1, $2, NULLIF($3,'')::uuid, $4, $5)
	`, uuid.NewString(), AuditActionAgentFreeGrantRevoke, actorID, detailsRaw, now); err != nil {
		return AgentFreeGrant{}, err
	}

	if err := tx.Commit(); err != nil {
		return AgentFreeGrant{}, err
	}
	return revoked, nil
}
