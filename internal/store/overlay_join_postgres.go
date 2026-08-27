package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func insertOverlayJoinAuditTx(ctx context.Context, tx *sql.Tx, audit *AuditLog) error {
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	details, err := json.Marshal(audit.Details)
	if err != nil {
		return err
	}
	var actor any
	if audit.ActorUUID != "" {
		actor = audit.ActorUUID
	}
	return tx.QueryRowContext(ctx, `
INSERT INTO public.audit_logs (uuid, action, actor_uuid, details)
VALUES ($1, $2, $3, $4::jsonb) RETURNING created_at`, audit.UUID, audit.Action, actor, string(details)).Scan(&audit.CreatedAt)
}

func (s *postgresStore) CreateOverlayJoinToken(ctx context.Context, token *OverlayJoinToken, audit *AuditLog) error {
	if token == nil || token.ID == "" || len(token.TokenHash) != sha256.Size || token.RemainingUses != 1 {
		return errors.New("valid overlay join token is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `
INSERT INTO public.overlay_join_tokens
  (id, token_hash, user_uuid, network_id, device_id, platform, remaining_uses, expires_at)
VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8)
RETURNING created_at`, token.ID, token.TokenHash, token.UserID, token.NetworkID, token.DeviceID, token.Platform, token.RemainingUses, token.ExpiresAt).Scan(&token.CreatedAt)
	if err != nil {
		return err
	}
	if err := insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) RevokeOverlayJoinToken(ctx context.Context, userID, tokenID string, revokedAt time.Time, audit *AuditLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE public.overlay_join_tokens SET revoked_at = COALESCE(revoked_at, $3)
WHERE id = $1 AND user_uuid = $2`, tokenID, userID, revokedAt)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrOverlayJoinTokenNotFound
	}
	if err := insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) ExchangeOverlayJoinToken(ctx context.Context, exchange *OverlayJoinExchange, audit *AuditLog) error {
	if exchange == nil || len(exchange.JoinTokenHash) != sha256.Size || len(exchange.Enrollment.TokenHash) != sha256.Size {
		return errors.New("valid overlay join exchange is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var token OverlayJoinToken
	var deviceConstraint, platformConstraint sql.NullString
	var revokedAt, lastExchangedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
SELECT id, user_uuid::text, network_id, device_id, platform, remaining_uses,
       expires_at, revoked_at, created_at, last_exchanged_at
FROM public.overlay_join_tokens WHERE token_hash = $1 FOR UPDATE`, exchange.JoinTokenHash).Scan(
		&token.ID, &token.UserID, &token.NetworkID, &deviceConstraint, &platformConstraint,
		&token.RemainingUses, &token.ExpiresAt, &revokedAt, &token.CreatedAt, &lastExchangedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOverlayJoinTokenNotFound
	}
	if err != nil {
		return err
	}
	token.DeviceID, token.Platform = deviceConstraint.String, platformConstraint.String
	if revokedAt.Valid {
		value := revokedAt.Time.UTC()
		token.RevokedAt = &value
	}
	if lastExchangedAt.Valid {
		value := lastExchangedAt.Time.UTC()
		token.LastExchangedAt = &value
	}
	now := exchange.Enrollment.CreatedAt.UTC()
	if err := validateJoinTokenForExchange(&token, exchange, now); err != nil {
		return err
	}
	var replayed bool
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM public.overlay_enrollment_sessions WHERE join_token_id = $1 AND device_id = $2)`, token.ID, exchange.Device.ID).Scan(&replayed); err != nil {
		return err
	}
	if replayed {
		return ErrOverlayJoinReplay
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-network:' || $1, 0))`, token.NetworkID); err != nil {
		return fmt.Errorf("lock overlay network address pool: %w", err)
	}

	var existing OverlayDevice
	err = tx.QueryRowContext(ctx, `
SELECT network_id, platform, wireguard_public_key, wireguard_address
FROM public.overlay_devices WHERE user_uuid = $1 AND id = $2 FOR UPDATE`, token.UserID, exchange.Device.ID).Scan(
		&existing.NetworkID, &existing.Platform, &existing.WireGuardPublicKey, &existing.WireGuardAddress,
	)
	switch {
	case err == nil:
		if existing.NetworkID != token.NetworkID || existing.Platform != exchange.Device.Platform || existing.WireGuardPublicKey != exchange.Device.WireGuardPublicKey {
			return ErrOverlayJoinDeviceConflict
		}
		exchange.Device.WireGuardAddress = existing.WireGuardAddress
	case errors.Is(err, sql.ErrNoRows):
		rows, err := tx.QueryContext(ctx, `SELECT wireguard_address FROM public.overlay_devices WHERE network_id = $1`, token.NetworkID)
		if err != nil {
			return err
		}
		used := make(map[string]bool)
		for rows.Next() {
			var address string
			if err := rows.Scan(&address); err != nil {
				rows.Close()
				return err
			}
			used[address] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
		address, err := allocateOverlayJoinAddress(token.UserID, exchange.Device.ID, exchange.AddressPrefix, exchange.AddressStartHost, exchange.AddressEndHost, used)
		if err != nil {
			return err
		}
		exchange.Device.WireGuardAddress = address
	default:
		return err
	}

	exchange.Device.UserID = token.UserID
	exchange.Device.NetworkID = token.NetworkID
	lastSeen := now
	exchange.Device.LastSeenAt = &lastSeen
	err = tx.QueryRowContext(ctx, `
INSERT INTO public.overlay_devices
  (id, user_uuid, network_id, name, platform, hostname, wireguard_public_key, wireguard_address, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (user_uuid, id) DO UPDATE SET
  name = EXCLUDED.name, hostname = EXCLUDED.hostname, last_seen_at = EXCLUDED.last_seen_at, updated_at = now()
WHERE overlay_devices.network_id = EXCLUDED.network_id
  AND overlay_devices.platform = EXCLUDED.platform
  AND overlay_devices.wireguard_public_key = EXCLUDED.wireguard_public_key
RETURNING created_at, updated_at`, exchange.Device.ID, token.UserID, token.NetworkID,
		exchange.Device.Name, exchange.Device.Platform, exchange.Device.Hostname, exchange.Device.WireGuardPublicKey,
		exchange.Device.WireGuardAddress, exchange.Device.LastSeenAt).Scan(&exchange.Device.CreatedAt, &exchange.Device.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// A differently scoped token may concurrently register the same user/device
		// while this transaction waits on its INSERT. The conflict WHERE clause is
		// the final identity check; never consume the invite on mismatch.
		return ErrOverlayJoinDeviceConflict
	}
	if err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx, `
UPDATE public.overlay_join_tokens
SET remaining_uses = remaining_uses - 1, last_exchanged_at = $2
WHERE id = $1 AND remaining_uses > 0`, token.ID, now)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrOverlayJoinTokenExhausted
	}
	exchange.Enrollment.JoinTokenID = token.ID
	exchange.Enrollment.UserID = token.UserID
	exchange.Enrollment.NetworkID = token.NetworkID
	exchange.Enrollment.DeviceID = exchange.Device.ID
	exchange.Enrollment.Platform = exchange.Device.Platform
	exchange.Enrollment.WireGuardPublicKey = exchange.Device.WireGuardPublicKey
	_, err = tx.ExecContext(ctx, `
INSERT INTO public.overlay_enrollment_sessions
  (id, token_hash, join_token_id, user_uuid, network_id, device_id, platform,
   wireguard_public_key, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, exchange.Enrollment.ID,
		exchange.Enrollment.TokenHash, token.ID, token.UserID, token.NetworkID, exchange.Device.ID,
		exchange.Device.Platform, exchange.Device.WireGuardPublicKey, exchange.Enrollment.ExpiresAt, now)
	if err != nil {
		return err
	}
	if audit.Details == nil {
		audit.Details = map[string]any{}
	}
	audit.Details["target_uuid"] = token.UserID
	audit.Details["join_token_id"] = token.ID
	audit.Details["enrollment_id"] = exchange.Enrollment.ID
	if err := insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetOverlayEnrollmentSession(ctx context.Context, tokenHash []byte, now time.Time) (*OverlayEnrollmentSession, error) {
	var session OverlayEnrollmentSession
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
UPDATE public.overlay_enrollment_sessions SET last_used_at = $2
WHERE token_hash = $1 AND expires_at > $2
RETURNING id, join_token_id, user_uuid::text, network_id, device_id, platform,
          wireguard_public_key, expires_at, created_at, last_used_at`, tokenHash, now.UTC()).Scan(
		&session.ID, &session.JoinTokenID, &session.UserID, &session.NetworkID, &session.DeviceID,
		&session.Platform, &session.WireGuardPublicKey, &session.ExpiresAt, &session.CreatedAt, &lastUsed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Do not reveal whether a hash existed but expired.
		return nil, ErrOverlayEnrollmentNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		value := lastUsed.Time.UTC()
		session.LastUsedAt = &value
	}
	return &session, nil
}

func prepareOverlayJoinAudit(action, actor string, details map[string]any) *AuditLog {
	return &AuditLog{UUID: uuid.NewString(), Action: action, ActorUUID: strings.TrimSpace(actor), Details: details}
}
