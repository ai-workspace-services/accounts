package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

func cloneAuditLog(entry *AuditLog) *AuditLog {
	if entry == nil {
		return nil
	}
	clone := *entry
	if entry.Details != nil {
		details := make(map[string]any, len(entry.Details))
		for k, v := range entry.Details {
			details[k] = v
		}
		clone.Details = details
	} else {
		clone.Details = map[string]any{}
	}
	return &clone
}

func (s *memoryStore) InsertAuditLog(ctx context.Context, entry *AuditLog) error {
	_ = ctx
	if entry == nil {
		return errors.New("audit entry is required")
	}
	if strings.TrimSpace(entry.Action) == "" {
		return errors.New("audit action is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(entry.UUID) == "" {
		entry.UUID = uuid.NewString()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	s.auditLogs = append(s.auditLogs, cloneAuditLog(entry))
	return nil
}

func (s *memoryStore) ListAuditLogs(ctx context.Context, filter AuditLogFilter) ([]AuditLog, error) {
	_ = ctx

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

	s.mu.RLock()
	defer s.mu.RUnlock()

	prefix := strings.TrimSpace(filter.ActionPrefix)
	actor := strings.TrimSpace(filter.ActorUUID)
	target := strings.TrimSpace(filter.TargetUUID)

	// Newest first, matching the postgres ORDER BY created_at DESC.
	matched := make([]AuditLog, 0, len(s.auditLogs))
	for i := len(s.auditLogs) - 1; i >= 0; i-- {
		entry := s.auditLogs[i]
		if entry == nil {
			continue
		}
		if prefix != "" && !strings.HasPrefix(entry.Action, prefix) {
			continue
		}
		if actor != "" && entry.ActorUUID != actor {
			continue
		}
		if target != "" {
			value, _ := entry.Details["target_uuid"].(string)
			if value != target {
				continue
			}
		}
		matched = append(matched, *cloneAuditLog(entry))
	}

	if offset >= len(matched) {
		return []AuditLog{}, nil
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], nil
}
