package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Bound the page size so an operator (or a bug) cannot ask for the whole
// table. audit_logs grows with every operator write and has already caused an
// IO incident in UAT once; unbounded reads are how that repeats.
const (
	auditLogDefaultLimit = 50
	auditLogMaxLimit     = 200
)

func (s *postgresStore) InsertAuditLog(ctx context.Context, entry *AuditLog) error {
	if entry == nil {
		return errors.New("audit entry is required")
	}
	action := strings.TrimSpace(entry.Action)
	if action == "" {
		return errors.New("audit action is required")
	}
	if strings.TrimSpace(entry.UUID) == "" {
		entry.UUID = uuid.NewString()
	}

	details := entry.Details
	if details == nil {
		details = map[string]any{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit details: %w", err)
	}

	// actor_uuid is a nullable UUID column, so an empty string has to go in as
	// NULL rather than "". A system-initiated change legitimately has no actor.
	var actor any
	if trimmed := strings.TrimSpace(entry.ActorUUID); trimmed != "" {
		actor = trimmed
	}

	const query = `
		INSERT INTO audit_logs (uuid, action, actor_uuid, details)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`

	return s.db.QueryRowContext(ctx, query, entry.UUID, action, actor, encoded).
		Scan(&entry.CreatedAt)
}

func (s *postgresStore) ListAuditLogs(ctx context.Context, filter AuditLogFilter) ([]AuditLog, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = auditLogDefaultLimit
	}
	if limit > auditLogMaxLimit {
		limit = auditLogMaxLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	// Built positionally so every value stays a bind parameter; the prefix goes
	// through LIKE with an escaped literal rather than string concatenation.
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 5)

	if prefix := strings.TrimSpace(filter.ActionPrefix); prefix != "" {
		args = append(args, escapeLikePrefix(prefix)+"%")
		conditions = append(conditions, fmt.Sprintf("action LIKE $%d ESCAPE '\\'", len(args)))
	}
	if actor := strings.TrimSpace(filter.ActorUUID); actor != "" {
		args = append(args, actor)
		conditions = append(conditions, fmt.Sprintf("actor_uuid = $%d", len(args)))
	}
	// target_uuid lives inside the JSONB payload rather than in a column, so
	// "everything ever done to this account" is a details lookup.
	if target := strings.TrimSpace(filter.TargetUUID); target != "" {
		args = append(args, target)
		conditions = append(conditions, fmt.Sprintf("details->>'target_uuid' = $%d", len(args)))
	}

	query := `SELECT uuid, action, COALESCE(actor_uuid::text, ''), details, created_at FROM audit_logs`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))
	args = append(args, offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]AuditLog, 0, limit)
	for rows.Next() {
		var (
			entry   AuditLog
			details []byte
		)
		if err := rows.Scan(&entry.UUID, &entry.Action, &entry.ActorUUID, &details, &entry.CreatedAt); err != nil {
			return nil, err
		}
		if len(details) > 0 {
			if err := json.Unmarshal(details, &entry.Details); err != nil {
				return nil, fmt.Errorf("decode audit details for %s: %w", entry.UUID, err)
			}
		}
		if entry.Details == nil {
			entry.Details = map[string]any{}
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// escapeLikePrefix neutralises LIKE wildcards so a filter of "billing_" means
// a literal underscore instead of "any character".
func escapeLikePrefix(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}
