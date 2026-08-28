package store

import (
	"context"
	"strings"
)

func (s *postgresStore) MarkOverlayPolicyReconcilePending(ctx context.Context, networkID, lastError string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO public.overlay_policy_reconcile_pending(network_id,attempts,last_error,updated_at) VALUES($1,1,$2,now()) ON CONFLICT(network_id) DO UPDATE SET attempts=overlay_policy_reconcile_pending.attempts+1,last_error=EXCLUDED.last_error,updated_at=now()`, strings.TrimSpace(networkID), strings.TrimSpace(lastError))
	return err
}
func (s *postgresStore) ClearOverlayPolicyReconcilePending(ctx context.Context, networkID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM public.overlay_policy_reconcile_pending WHERE network_id=$1`, strings.TrimSpace(networkID))
	return err
}
func (s *postgresStore) ListOverlayPolicyReconcilePending(ctx context.Context, limit int) ([]OverlayPolicyReconcilePending, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT network_id,attempts,last_error,updated_at FROM public.overlay_policy_reconcile_pending ORDER BY updated_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OverlayPolicyReconcilePending{}
	for rows.Next() {
		var item OverlayPolicyReconcilePending
		if err = rows.Scan(&item.NetworkID, &item.Attempts, &item.LastError, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.UpdatedAt = item.UpdatedAt.UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}
