package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *postgresStore) OverlayProjectionDurable() bool { return true }

func (s *postgresStore) GetOverlaySigningKeyMaxExpiresAt(ctx context.Context, keyID string) (time.Time, error) {
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(MAX(expires_at), 'epoch'::timestamptz)
FROM (
  SELECT expires_at FROM public.overlay_signed_configs WHERE signing_key_id = $1
  UNION ALL
  SELECT expires_at FROM public.overlay_gateway_snapshots WHERE signing_key_id = $1
) signing_windows`, keyID).Scan(&expiresAt)
	return expiresAt.UTC(), err
}

const latestOverlaySignedConfigQuery = `
SELECT c.user_uuid::text, c.device_id, c.config_id, c.network_id, c.generation,
       c.source_revision, c.signing_key_id, c.signed_payload, c.issued_at,
       c.expires_at, c.created_at, a.config_id, a.applied_at, a.received_at
FROM public.overlay_signed_configs c
LEFT JOIN public.overlay_signed_config_acks a
  ON a.user_uuid = c.user_uuid AND a.device_id = c.device_id AND a.generation = c.generation
WHERE c.user_uuid = $1 AND c.device_id = $2
ORDER BY c.generation DESC
LIMIT 1`

func scanOverlaySignedConfig(row interface{ Scan(...any) error }) (*OverlaySignedConfigRecord, error) {
	var record OverlaySignedConfigRecord
	var ackConfigID sql.NullString
	var ackAppliedAt, ackReceivedAt sql.NullTime
	if err := row.Scan(
		&record.UserID, &record.DeviceID, &record.ConfigID, &record.NetworkID,
		&record.Generation, &record.SourceRevision, &record.SigningKeyID,
		&record.SignedPayload, &record.IssuedAt, &record.ExpiresAt, &record.CreatedAt,
		&ackConfigID, &ackAppliedAt, &ackReceivedAt,
	); err != nil {
		return nil, err
	}
	if ackConfigID.Valid && ackAppliedAt.Valid && ackReceivedAt.Valid {
		record.Ack = &OverlaySignedConfigAck{
			UserID: record.UserID, DeviceID: record.DeviceID, ConfigID: ackConfigID.String,
			Generation: record.Generation, AppliedAt: ackAppliedAt.Time.UTC(), ReceivedAt: ackReceivedAt.Time.UTC(),
		}
	}
	record.IssuedAt = record.IssuedAt.UTC()
	record.ExpiresAt = record.ExpiresAt.UTC()
	record.CreatedAt = record.CreatedAt.UTC()
	return &record, nil
}

func (s *postgresStore) GetLatestOverlaySignedConfig(ctx context.Context, userID, deviceID string) (*OverlaySignedConfigRecord, error) {
	record, err := scanOverlaySignedConfig(s.db.QueryRowContext(ctx, latestOverlaySignedConfigQuery, userID, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlaySignedConfigNotFound
	}
	return record, err
}

func lockOverlayProjection(ctx context.Context, tx *sql.Tx, userID, deviceID string) error {
	// The transaction-scoped advisory lock also serializes the first projection,
	// when no row exists yet for SELECT ... FOR UPDATE to lock.
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || chr(31) || $2, 0))`, userID, deviceID)
	return err
}

func (s *postgresStore) SaveOverlaySignedConfig(ctx context.Context, record *OverlaySignedConfigRecord) error {
	if record == nil {
		return errors.New("overlay signed config record is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockOverlayProjection(ctx, tx, record.UserID, record.DeviceID); err != nil {
		return fmt.Errorf("lock overlay projection: %w", err)
	}
	var currentGeneration uint64
	var currentSource string
	err = tx.QueryRowContext(ctx, `
SELECT generation, source_revision
FROM public.overlay_signed_configs
WHERE user_uuid = $1 AND device_id = $2
ORDER BY generation DESC LIMIT 1 FOR UPDATE`, record.UserID, record.DeviceID).Scan(&currentGeneration, &currentSource)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if record.Generation != 1 {
			return ErrOverlaySignedConfigGap
		}
	case err != nil:
		return err
	default:
		if currentSource == record.SourceRevision || record.Generation <= currentGeneration {
			return ErrOverlaySignedConfigStale
		}
		if record.Generation != currentGeneration+1 {
			return ErrOverlaySignedConfigGap
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO public.overlay_signed_configs
  (user_uuid, device_id, config_id, network_id, generation, source_revision,
   signing_key_id, signed_payload, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)`,
		record.UserID, record.DeviceID, record.ConfigID, record.NetworkID, record.Generation,
		record.SourceRevision, record.SigningKeyID, string(record.SignedPayload), record.IssuedAt, record.ExpiresAt,
	); err != nil {
		return fmt.Errorf("insert overlay signed config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *postgresStore) AcknowledgeOverlaySignedConfig(ctx context.Context, ack *OverlaySignedConfigAck) (bool, error) {
	if ack == nil {
		return false, errors.New("overlay signed config acknowledgement is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockOverlayProjection(ctx, tx, ack.UserID, ack.DeviceID); err != nil {
		return false, fmt.Errorf("lock overlay projection ACK: %w", err)
	}
	var latestGeneration uint64
	var configID string
	err = tx.QueryRowContext(ctx, `
SELECT generation, config_id FROM public.overlay_signed_configs
WHERE user_uuid = $1 AND device_id = $2
ORDER BY generation DESC LIMIT 1 FOR UPDATE`, ack.UserID, ack.DeviceID).Scan(&latestGeneration, &configID)
	if errors.Is(err, sql.ErrNoRows) || ack.Generation > latestGeneration {
		return false, ErrOverlaySignedConfigNotFound
	}
	if err != nil {
		return false, err
	}
	if ack.Generation < latestGeneration {
		return false, ErrOverlaySignedConfigStale
	}
	if ack.ConfigID != configID {
		return false, ErrOverlaySignedConfigMismatch
	}
	var appliedAt, receivedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
SELECT applied_at, received_at FROM public.overlay_signed_config_acks
WHERE user_uuid = $1 AND device_id = $2 AND generation = $3`, ack.UserID, ack.DeviceID, ack.Generation).Scan(&appliedAt, &receivedAt)
	if err == nil {
		ack.AppliedAt, ack.ReceivedAt = appliedAt.Time.UTC(), receivedAt.Time.UTC()
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `
INSERT INTO public.overlay_signed_config_acks
  (user_uuid, device_id, generation, config_id, applied_at, received_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING applied_at, received_at`, ack.UserID, ack.DeviceID, ack.Generation, ack.ConfigID, ack.AppliedAt, ack.ReceivedAt).Scan(&ack.AppliedAt, &ack.ReceivedAt); err != nil {
		return false, fmt.Errorf("insert overlay signed config ACK: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}
