package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

func (s *postgresStore) GetOverlayNode(ctx context.Context, nodeID string) (*OverlayNode, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, network_id, name, role, region, wireguard_public_key,
       wireguard_address, endpoint_host, endpoint_port, transport_type,
       transport_security, transport_path, transport_mode, transport_uuid,
       healthy, created_at, updated_at, last_heartbeat
FROM public.overlay_nodes WHERE id = $1`, strings.TrimSpace(nodeID))
	node, err := scanOverlayNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayNodeNotFound
	}
	return node, err
}

func (s *postgresStore) CreateOverlayNodeCredential(ctx context.Context, credential *OverlayNodeCredential, audit *AuditLog) error {
	if credential == nil || credential.ID == "" || credential.NodeID == "" || len(credential.TokenHash) != sha256.Size {
		return errors.New("valid overlay node credential is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `
INSERT INTO public.overlay_node_credentials (id, node_id, token_hash, expires_at)
VALUES ($1,$2,$3,$4) RETURNING created_at`, credential.ID, credential.NodeID, credential.TokenHash, credential.ExpiresAt).Scan(&credential.CreatedAt)
	if err != nil {
		return err
	}
	if err := insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) RevokeOverlayNodeCredential(ctx context.Context, nodeID, credentialID string, revokedAt time.Time, audit *AuditLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE public.overlay_node_credentials SET revoked_at=COALESCE(revoked_at,$3) WHERE id=$1 AND node_id=$2`, credentialID, nodeID, revokedAt)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrOverlayNodeCredentialNotFound
	}
	if err := insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) AuthenticateOverlayNodeCredential(ctx context.Context, tokenHash []byte, now time.Time) (*OverlayNodeCredential, error) {
	var credential OverlayNodeCredential
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
UPDATE public.overlay_node_credentials SET last_used_at=$2
WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>$2
RETURNING id,node_id,expires_at,revoked_at,created_at,last_used_at`, tokenHash, now.UTC()).Scan(
		&credential.ID, &credential.NodeID, &credential.ExpiresAt, &credential.RevokedAt, &credential.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayNodeCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		value := lastUsed.Time.UTC()
		credential.LastUsedAt = &value
	}
	credential.ExpiresAt, credential.CreatedAt = credential.ExpiresAt.UTC(), credential.CreatedAt.UTC()
	return &credential, nil
}

func (s *postgresStore) RecordOverlayGatewayHeartbeat(ctx context.Context, heartbeat *OverlayGatewayHeartbeat) error {
	if heartbeat == nil {
		return errors.New("valid overlay gateway heartbeat is required")
	}
	err := s.db.QueryRowContext(ctx, `
INSERT INTO public.overlay_gateway_node_status
 (node_id,agent_version,mode,proxy_core,observed_generation,applied_generation,received_at)
VALUES ($1,$2,$3,$4,$5,$6,now())
ON CONFLICT (node_id) DO UPDATE SET agent_version=EXCLUDED.agent_version, mode=EXCLUDED.mode,
 proxy_core=EXCLUDED.proxy_core, observed_generation=EXCLUDED.observed_generation,
 applied_generation=EXCLUDED.applied_generation, received_at=now()
WHERE overlay_gateway_node_status.observed_generation<=EXCLUDED.observed_generation
 AND overlay_gateway_node_status.applied_generation<=EXCLUDED.applied_generation
RETURNING received_at`, heartbeat.NodeID, heartbeat.AgentVersion, heartbeat.Mode, heartbeat.ProxyCore, heartbeat.ObservedGeneration, heartbeat.AppliedGeneration).Scan(&heartbeat.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOverlayGatewayReportStale
	}
	return err
}

func scanOverlayGatewaySnapshot(row interface{ Scan(...any) error }) (*OverlayGatewaySnapshotRecord, error) {
	var record OverlayGatewaySnapshotRecord
	err := row.Scan(&record.NodeID, &record.SnapshotID, &record.Generation, &record.ExpectedPreviousGeneration, &record.SourceRevision, &record.SigningKeyID, &record.SignedPayload, &record.IssuedAt, &record.ExpiresAt, &record.CreatedAt)
	if err != nil {
		return nil, err
	}
	record.SignedPayload = append([]byte(nil), record.SignedPayload...)
	record.IssuedAt, record.ExpiresAt, record.CreatedAt = record.IssuedAt.UTC(), record.ExpiresAt.UTC(), record.CreatedAt.UTC()
	return &record, nil
}

const latestGatewaySnapshotSQL = `SELECT node_id,snapshot_id,generation,expected_previous_generation,source_revision,signing_key_id,signed_payload,issued_at,expires_at,created_at FROM public.overlay_gateway_snapshots WHERE node_id=$1 ORDER BY generation DESC LIMIT 1`

func (s *postgresStore) GetLatestOverlayGatewaySnapshot(ctx context.Context, nodeID string) (*OverlayGatewaySnapshotRecord, error) {
	record, err := scanOverlayGatewaySnapshot(s.db.QueryRowContext(ctx, latestGatewaySnapshotSQL, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayGatewaySnapshotNotFound
	}
	return record, err
}

func (s *postgresStore) SaveOverlayGatewaySnapshot(ctx context.Context, record *OverlayGatewaySnapshotRecord) error {
	if record == nil || record.Generation == 0 || len(record.SignedPayload) == 0 {
		return errors.New("valid overlay gateway snapshot is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-gateway:' || $1,0))`, record.NodeID); err != nil {
		return err
	}
	latest, err := scanOverlayGatewaySnapshot(tx.QueryRowContext(ctx, latestGatewaySnapshotSQL+` FOR UPDATE`, record.NodeID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if record.Generation != 1 || record.ExpectedPreviousGeneration != 0 {
			return ErrOverlayGatewayGenerationStale
		}
	case err != nil:
		return err
	default:
		if latest.SourceRevision == record.SourceRevision {
			return ErrOverlayGatewaySourceExists
		}
		if record.Generation != latest.Generation+1 || record.ExpectedPreviousGeneration != latest.Generation {
			return ErrOverlayGatewayGenerationStale
		}
	}
	err = tx.QueryRowContext(ctx, `
INSERT INTO public.overlay_gateway_snapshots
 (node_id,snapshot_id,generation,expected_previous_generation,source_revision,signing_key_id,signed_payload,issued_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9) RETURNING created_at`, record.NodeID, record.SnapshotID, record.Generation, record.ExpectedPreviousGeneration, record.SourceRevision, record.SigningKeyID, string(record.SignedPayload), record.IssuedAt, record.ExpiresAt).Scan(&record.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) RecordOverlayGatewayApplyResult(ctx context.Context, result *OverlayGatewayApplyResult) (bool, error) {
	if result == nil {
		return false, errors.New("valid overlay gateway apply result is required")
	}
	diff := json.RawMessage(result.Diff)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var generation uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM public.overlay_gateway_snapshots WHERE node_id=$1 AND snapshot_id=$2 FOR UPDATE`, result.NodeID, result.SnapshotID).Scan(&generation); errors.Is(err, sql.ErrNoRows) {
		return false, ErrOverlayGatewayReportStale
	} else if err != nil {
		return false, err
	}
	if generation != result.ObservedGeneration {
		return false, ErrOverlayGatewayReportStale
	}
	var previous OverlayGatewayApplyResult
	var previousDiff []byte
	err = tx.QueryRowContext(ctx, `SELECT observed_generation,applied_generation,runtime_applied,result,diff,received_at FROM public.overlay_gateway_apply_results WHERE node_id=$1 AND snapshot_id=$2`, result.NodeID, result.SnapshotID).Scan(&previous.ObservedGeneration, &previous.AppliedGeneration, &previous.RuntimeApplied, &previous.Result, &previousDiff, &previous.ReceivedAt)
	if err == nil {
		var previousValue, currentValue any
		previousJSONErr, currentJSONErr := json.Unmarshal(previousDiff, &previousValue), json.Unmarshal(result.Diff, &currentValue)
		if previous.ObservedGeneration == result.ObservedGeneration && previous.AppliedGeneration == result.AppliedGeneration && previous.RuntimeApplied == result.RuntimeApplied && previous.Result == result.Result && previousJSONErr == nil && currentJSONErr == nil && reflect.DeepEqual(previousValue, currentValue) {
			result.ReceivedAt = previous.ReceivedAt.UTC()
			return true, tx.Commit()
		}
		return false, ErrOverlayGatewayReportStale
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO public.overlay_gateway_apply_results (node_id,snapshot_id,observed_generation,applied_generation,runtime_applied,result,diff) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb) RETURNING received_at`, result.NodeID, result.SnapshotID, result.ObservedGeneration, result.AppliedGeneration, result.RuntimeApplied, result.Result, string(diff)).Scan(&result.ReceivedAt)
	if err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func (s *postgresStore) ListOverlayProjectionDevicesByNetwork(ctx context.Context, networkID string) ([]OverlayProjectionDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id,d.user_uuid,d.network_id,d.name,d.platform,d.hostname,d.wireguard_public_key,d.wireguard_address,d.created_at,d.updated_at,d.last_seen_at,
 COALESCE(m.tags,'[]'::jsonb),COALESCE(m.attachments,'[]'::jsonb)
FROM public.overlay_devices d LEFT JOIN public.overlay_device_projection_metadata m ON m.user_uuid=d.user_uuid AND m.device_id=d.id
WHERE d.network_id=$1 ORDER BY d.id,d.user_uuid`, networkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []OverlayProjectionDevice
	for rows.Next() {
		var item OverlayProjectionDevice
		var lastSeen sql.NullTime
		var tagsRaw, attachmentsRaw []byte
		if err := rows.Scan(&item.Device.ID, &item.Device.UserID, &item.Device.NetworkID, &item.Device.Name, &item.Device.Platform, &item.Device.Hostname, &item.Device.WireGuardPublicKey, &item.Device.WireGuardAddress, &item.Device.CreatedAt, &item.Device.UpdatedAt, &lastSeen, &tagsRaw, &attachmentsRaw); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			value := lastSeen.Time.UTC()
			item.Device.LastSeenAt = &value
		}
		if err := json.Unmarshal(tagsRaw, &item.Tags); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(attachmentsRaw, &item.Attachments); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *postgresStore) ImportOverlayStaticClients(ctx context.Context, input *OverlayStaticImport, audit *AuditLog) (*OverlayStaticImportReceipt, bool, error) {
	if input == nil || !strings.HasPrefix(input.IdempotencyKey, "sha256-") || len(input.IdempotencyKey) != 71 || len(input.BodySHA256) != 64 || len(input.Devices) == 0 {
		return nil, false, errors.New("valid overlay static import is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-static-import',0))`); err != nil {
		return nil, false, err
	}
	var receipt OverlayStaticImportReceipt
	var storedHash string
	err = tx.QueryRowContext(ctx, `SELECT import_id,idempotency_key,body_sha256,owner_user_uuid::text,network_id,baseline_sha256,device_count,created_at FROM public.overlay_static_import_receipts WHERE idempotency_key=$1`, input.IdempotencyKey).Scan(&receipt.ImportID, &receipt.IdempotencyKey, &storedHash, &receipt.OwnerUserID, &receipt.NetworkID, &receipt.BaselineSHA256, &receipt.DeviceCount, &receipt.CreatedAt)
	if err == nil {
		if storedHash != input.BodySHA256 {
			return nil, false, ErrOverlayStaticImportIdempotency
		}
		receipt.CreatedAt = receipt.CreatedAt.UTC()
		return &receipt, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var ownerExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.users WHERE uuid=$1)`, input.OwnerUserID).Scan(&ownerExists); err != nil {
		return nil, false, err
	}
	if !ownerExists {
		return nil, false, ErrUserNotFound
	}
	for _, projected := range input.Devices {
		var userID, networkID, key, address string
		err := tx.QueryRowContext(ctx, `SELECT user_uuid::text,network_id,wireguard_public_key,wireguard_address FROM public.overlay_devices WHERE id=$1 FOR UPDATE`, projected.Device.ID).Scan(&userID, &networkID, &key, &address)
		if err == nil && (userID != input.OwnerUserID || networkID != input.NetworkID || key != projected.Device.WireGuardPublicKey || address != projected.Device.WireGuardAddress) {
			return nil, false, ErrOverlayStaticImportConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		var collision bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.overlay_devices WHERE network_id=$1 AND id<>$2 AND (wireguard_public_key=$3 OR wireguard_address=$4))`, input.NetworkID, projected.Device.ID, projected.Device.WireGuardPublicKey, projected.Device.WireGuardAddress).Scan(&collision); err != nil {
			return nil, false, err
		}
		if collision {
			return nil, false, ErrOverlayStaticImportConflict
		}
	}
	for _, projected := range input.Devices {
		device := projected.Device
		if device.Name == "" {
			device.Name = device.ID
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO public.overlay_devices (id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (user_uuid,id) DO UPDATE SET name=EXCLUDED.name,platform=EXCLUDED.platform,hostname=EXCLUDED.hostname,updated_at=now() WHERE overlay_devices.network_id=EXCLUDED.network_id AND overlay_devices.wireguard_public_key=EXCLUDED.wireguard_public_key`, device.ID, input.OwnerUserID, input.NetworkID, device.Name, device.Platform, device.Hostname, device.WireGuardPublicKey, device.WireGuardAddress)
		if err != nil {
			return nil, false, err
		}
		tags, _ := json.Marshal(projected.Tags)
		attachments, _ := json.Marshal(projected.Attachments)
		_, err = tx.ExecContext(ctx, `INSERT INTO public.overlay_device_projection_metadata (user_uuid,device_id,tags,attachments,source_kind,source_variable,baseline_sha256) VALUES ($1,$2,$3::jsonb,$4::jsonb,$5,$6,$7) ON CONFLICT (user_uuid,device_id) DO UPDATE SET tags=EXCLUDED.tags,attachments=EXCLUDED.attachments,source_kind=EXCLUDED.source_kind,source_variable=EXCLUDED.source_variable,baseline_sha256=EXCLUDED.baseline_sha256,updated_at=now()`, input.OwnerUserID, device.ID, string(tags), string(attachments), input.SourceKind, input.SourceVariable, input.BaselineSHA256)
		if err != nil {
			return nil, false, err
		}
	}
	receipt = OverlayStaticImportReceipt{ImportID: "import_" + input.BodySHA256[:24], IdempotencyKey: input.IdempotencyKey, OwnerUserID: input.OwnerUserID, NetworkID: input.NetworkID, BaselineSHA256: input.BaselineSHA256, DeviceCount: len(input.Devices)}
	err = tx.QueryRowContext(ctx, `INSERT INTO public.overlay_static_import_receipts (import_id,idempotency_key,body_sha256,owner_user_uuid,network_id,source_kind,source_variable,baseline_sha256,device_count) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`, receipt.ImportID, receipt.IdempotencyKey, input.BodySHA256, input.OwnerUserID, input.NetworkID, input.SourceKind, input.SourceVariable, input.BaselineSHA256, receipt.DeviceCount).Scan(&receipt.CreatedAt)
	if err != nil {
		return nil, false, err
	}
	if audit.Details == nil {
		audit.Details = map[string]any{}
	}
	audit.Details["import_id"] = receipt.ImportID
	if err := insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &receipt, false, nil
}
