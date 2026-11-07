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

// EmailOptionalRegistrationGroup records the policy used when creating a new
// account. It grants ordinary access, without asserting email ownership.
const EmailOptionalRegistrationGroup = "account:registration:email-optional"

func AccountAccessVerified(user *User) bool {
	if user == nil {
		return false
	}
	if user.EmailVerified {
		return true
	}
	for _, group := range user.Groups {
		if group == EmailOptionalRegistrationGroup {
			return true
		}
	}
	return false
}

// Generic group edits cannot grant or remove the registration policy marker
// or temporary Free node grants.
func PreserveRegistrationGroups(existing, requested []string) []string {
	result := make([]string, 0, len(requested)+1)
	for _, group := range requested {
		if group != EmailOptionalRegistrationGroup {
			result = append(result, group)
		}
	}
	for _, group := range existing {
		if group == EmailOptionalRegistrationGroup {
			result = append(result, group)
			break
		}
	}
	return PreserveAgentFreeGrantGroups(existing, result)
}

func activeStateAudit(id, actor string, before, active bool) *AuditLog {
	action := "account.user.deactivated"
	if active {
		action = "account.user.activated"
	}
	return &AuditLog{UUID: uuid.NewString(), Action: action, ActorUUID: actor, CreatedAt: time.Now().UTC(), Details: map[string]any{"targetAccountUuid": id, "before": map[string]any{"active": before}, "after": map[string]any{"active": active}, "method": "administrator"}}
}

func (s *memoryStore) SetUserActiveWithAudit(ctx context.Context, id string, active bool, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("administrator actor required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	if active && user.ArchivedAt != nil {
		return ErrUserAlreadyArchived
	}
	if user.Active == active {
		return nil
	}
	entry := activeStateAudit(id, actor, user.Active, active)
	user.Active = active
	user.UpdatedAt = entry.CreatedAt
	s.auditLogs = append(s.auditLogs, entry)
	return nil
}

func (s *postgresStore) SetUserActiveWithAudit(ctx context.Context, id string, active bool, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("administrator actor required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before bool
	var archived sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT active, archived_at FROM users WHERE uuid = $1 FOR UPDATE`, id).Scan(&before, &archived); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	if active && archived.Valid {
		return ErrUserAlreadyArchived
	}
	if before == active {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET active = $1, updated_at = now() WHERE uuid = $2`, active, id); err != nil {
		return err
	}
	entry := activeStateAudit(id, actor, before, active)
	details, err := json.Marshal(entry.Details)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs (uuid, action, actor_uuid, details) VALUES ($1,$2,$3,$4)`, entry.UUID, entry.Action, entry.ActorUUID, details); err != nil {
		return err
	}
	return tx.Commit()
}
