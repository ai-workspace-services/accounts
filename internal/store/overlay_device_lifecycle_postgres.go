package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *postgresStore) lifecycleDeviceForUpdate(ctx context.Context, tx *sql.Tx, userID, networkID, deviceID string) (*OverlayDevice, error) {
	row := tx.QueryRowContext(ctx, `SELECT id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason FROM public.overlay_devices WHERE user_uuid=$1 AND network_id=$2 AND id=$3 FOR UPDATE`, userID, networkID, deviceID)
	return scanOverlayDevice(row)
}
func insertOverlayDeviceEventTx(ctx context.Context, tx *sql.Tx, d *OverlayDevice, eventType string) error {
	return tx.QueryRowContext(ctx, `INSERT INTO public.overlay_device_events(user_uuid,network_id,device_id,event_type,status,state_version,key_version) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING sequence,created_at`, d.UserID, d.NetworkID, d.ID, eventType, d.Status, d.StateVersion, d.KeyVersion).Scan(new(uint64), new(time.Time))
}
func (s *postgresStore) RotateOverlayDeviceKey(ctx context.Context, userID, networkID, deviceID, newKey string, expected uint64, audit *AuditLog) (*OverlayDevice, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	d, err := s.lifecycleDeviceForUpdate(ctx, tx, userID, networkID, deviceID)
	if err != nil {
		return nil, false, err
	}
	if d.Status == OverlayDeviceRevoked {
		return nil, false, ErrOverlayDeviceRevoked
	}
	newKey = strings.TrimSpace(newKey)
	if d.WireGuardPublicKey == newKey {
		return d, true, tx.Commit()
	}
	if expected == 0 || d.KeyVersion != expected {
		return nil, false, ErrOverlayDeviceVersionConflict
	}
	if err = claimOverlayDeviceKeyTx(ctx, tx, networkID, newKey, userID, deviceID, d.KeyVersion+1); err != nil {
		return nil, false, err
	}
	row := tx.QueryRowContext(ctx, `UPDATE public.overlay_devices SET wireguard_public_key=$4,key_version=key_version+1,state_version=state_version+1,updated_at=now() WHERE user_uuid=$1 AND network_id=$2 AND id=$3 RETURNING id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason`, userID, networkID, deviceID, newKey)
	if d, err = scanOverlayDevice(row); err != nil {
		return nil, false, err
	}
	if err = insertOverlayDeviceEventTx(ctx, tx, d, "key_rotated"); err != nil {
		return nil, false, err
	}
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return d, false, nil
}
func (s *postgresStore) SetOverlayDeviceStatus(ctx context.Context, userID, networkID, deviceID, status string, expected uint64, reason string, audit *AuditLog) (*OverlayDevice, bool, error) {
	if status != OverlayDeviceActive && status != OverlayDeviceInactive && status != OverlayDeviceRevoked {
		return nil, false, errors.New("invalid overlay device status")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	d, err := s.lifecycleDeviceForUpdate(ctx, tx, userID, networkID, deviceID)
	if err != nil {
		return nil, false, err
	}
	if d.Status == OverlayDeviceRevoked {
		if status == OverlayDeviceRevoked {
			return d, true, tx.Commit()
		}
		return nil, false, ErrOverlayDeviceRevoked
	}
	if d.Status == status {
		return d, true, tx.Commit()
	}
	if expected == 0 || d.StateVersion != expected {
		return nil, false, ErrOverlayDeviceVersionConflict
	}
	reason = strings.TrimSpace(reason)
	var revokedAt any
	if status == OverlayDeviceRevoked {
		revokedAt = time.Now().UTC()
	}
	row := tx.QueryRowContext(ctx, `UPDATE public.overlay_devices SET status=$4,state_version=state_version+1,revoked_at=CASE WHEN $4='revoked' THEN $5 ELSE NULL END,revoked_reason=CASE WHEN $4='revoked' THEN $6 ELSE '' END,updated_at=now() WHERE user_uuid=$1 AND network_id=$2 AND id=$3 RETURNING id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason`, userID, networkID, deviceID, status, revokedAt, reason)
	if d, err = scanOverlayDevice(row); err != nil {
		return nil, false, err
	}
	eventType := "status_changed"
	if status == OverlayDeviceRevoked {
		eventType = "revoked"
		if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_join_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3`, userID, networkID, deviceID); err != nil {
			return nil, false, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM public.overlay_enrollment_sessions WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3`, userID, networkID, deviceID); err != nil {
			return nil, false, err
		}
	}
	if err = insertOverlayDeviceEventTx(ctx, tx, d, eventType); err != nil {
		return nil, false, err
	}
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return d, false, nil
}
func (s *postgresStore) ListOverlayDeviceEvents(ctx context.Context, userID, networkID string, after uint64, limit int) ([]OverlayDeviceEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,user_uuid::text,network_id,device_id,event_type,status,state_version,key_version,created_at FROM public.overlay_device_events WHERE user_uuid=$1 AND ($2='' OR network_id=$2) AND sequence>$3 ORDER BY sequence LIMIT $4`, userID, networkID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OverlayDeviceEvent{}
	for rows.Next() {
		var event OverlayDeviceEvent
		if err = rows.Scan(&event.Sequence, &event.UserID, &event.NetworkID, &event.DeviceID, &event.Type, &event.Status, &event.StateVersion, &event.KeyVersion, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		out = append(out, event)
	}
	return out, rows.Err()
}
