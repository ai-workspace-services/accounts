package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *postgresStore) UpsertOverlayDevice(ctx context.Context, device *OverlayDevice) error {
	if device == nil {
		return errors.New("overlay device is required")
	}
	device.ID, device.UserID, device.NetworkID, device.WireGuardPublicKey = strings.TrimSpace(device.ID), strings.TrimSpace(device.UserID), strings.TrimSpace(device.NetworkID), strings.TrimSpace(device.WireGuardPublicKey)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-device-key:' || $1,0))`, device.NetworkID); err != nil {
		return err
	}
	existing, lookup := scanOverlayDevice(tx.QueryRowContext(ctx, `SELECT id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason FROM public.overlay_devices WHERE user_uuid=$1 AND id=$2 FOR UPDATE`, device.UserID, device.ID))
	if lookup == nil {
		if existing.Status == OverlayDeviceRevoked {
			return ErrOverlayDeviceRevoked
		}
		if existing.NetworkID != device.NetworkID {
			return ErrOverlayJoinConstraint
		}
		if existing.WireGuardPublicKey != device.WireGuardPublicKey {
			return ErrOverlayDeviceKeyConflict
		}
		row := tx.QueryRowContext(ctx, `UPDATE public.overlay_devices SET name=$3,platform=$4,hostname=$5,wireguard_address=$6,last_seen_at=$7,updated_at=now() WHERE user_uuid=$1 AND id=$2 RETURNING id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason`, device.UserID, device.ID, strings.TrimSpace(device.Name), strings.TrimSpace(device.Platform), strings.TrimSpace(device.Hostname), strings.TrimSpace(device.WireGuardAddress), device.LastSeenAt)
		stored, err := scanOverlayDevice(row)
		if err != nil {
			return err
		}
		*device = *stored
		return tx.Commit()
	}
	if !errors.Is(lookup, ErrOverlayDeviceNotFound) {
		return lookup
	}
	if err = claimOverlayDeviceKeyTx(ctx, tx, device.NetworkID, device.WireGuardPublicKey, device.UserID, device.ID, 1); err != nil {
		return err
	}
	row := tx.QueryRowContext(ctx, `INSERT INTO public.overlay_devices(id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,last_seen_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason`, device.ID, device.UserID, device.NetworkID, strings.TrimSpace(device.Name), strings.TrimSpace(device.Platform), strings.TrimSpace(device.Hostname), device.WireGuardPublicKey, strings.TrimSpace(device.WireGuardAddress), device.LastSeenAt)
	stored, err := scanOverlayDevice(row)
	if err != nil {
		return err
	}
	if err = insertOverlayDeviceEventTx(ctx, tx, stored, "registered"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	*device = *stored
	return nil
}

func claimOverlayDeviceKeyTx(ctx context.Context, tx *sql.Tx, networkID, publicKey, userID, deviceID string, keyVersion uint64) error {
	var claimed string
	err := tx.QueryRowContext(ctx, `INSERT INTO public.overlay_device_key_history(network_id,wireguard_public_key,user_uuid,device_id,key_version) VALUES($1,$2,$3,$4,$5) ON CONFLICT(network_id,wireguard_public_key) DO NOTHING RETURNING wireguard_public_key`, strings.TrimSpace(networkID), strings.TrimSpace(publicKey), strings.TrimSpace(userID), strings.TrimSpace(deviceID), keyVersion).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOverlayDeviceKeyConflict
	}
	return err
}

func (s *postgresStore) GetOverlayDevice(ctx context.Context, userID, deviceID string) (*OverlayDevice, error) {
	const query = `
		SELECT id, user_uuid, network_id, name, platform, hostname,
		       wireguard_public_key, wireguard_address, created_at, updated_at, last_seen_at,status,state_version,key_version,revoked_at,revoked_reason
		FROM overlay_devices
		WHERE user_uuid = $1 AND id = $2`
	row := s.db.QueryRowContext(ctx, query, strings.TrimSpace(userID), strings.TrimSpace(deviceID))
	return scanOverlayDevice(row)
}

func (s *postgresStore) ListOverlayDevicesByUser(ctx context.Context, userID string) ([]OverlayDevice, error) {
	const query = `
		SELECT id, user_uuid, network_id, name, platform, hostname,
		       wireguard_public_key, wireguard_address, created_at, updated_at, last_seen_at,status,state_version,key_version,revoked_at,revoked_reason
		FROM overlay_devices
		WHERE user_uuid = $1
		ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query, strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]OverlayDevice, 0)
	for rows.Next() {
		device, err := scanOverlayDevice(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *device)
	}
	return result, rows.Err()
}

func (s *postgresStore) ListOverlayDevicesByNetwork(ctx context.Context, networkID string) ([]OverlayDevice, error) {
	args := []any{}
	query := `
		SELECT id, user_uuid, network_id, name, platform, hostname,
		       wireguard_public_key, wireguard_address, created_at, updated_at, last_seen_at,status,state_version,key_version,revoked_at,revoked_reason
		FROM overlay_devices`
	if strings.TrimSpace(networkID) != "" {
		query += " WHERE network_id = $1"
		args = append(args, strings.TrimSpace(networkID))
	}
	query += " ORDER BY user_uuid ASC, id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]OverlayDevice, 0)
	for rows.Next() {
		device, err := scanOverlayDevice(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *device)
	}
	return result, rows.Err()
}

func scanOverlayDevice(row rowScanner) (*OverlayDevice, error) {
	var device OverlayDevice
	var lastSeen, revokedAt sql.NullTime
	err := row.Scan(
		&device.ID,
		&device.UserID,
		&device.NetworkID,
		&device.Name,
		&device.Platform,
		&device.Hostname,
		&device.WireGuardPublicKey,
		&device.WireGuardAddress,
		&device.CreatedAt,
		&device.UpdatedAt,
		&lastSeen,
		&device.Status, &device.StateVersion, &device.KeyVersion, &revokedAt, &device.RevokedReason,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOverlayDeviceNotFound
		}
		return nil, err
	}
	if lastSeen.Valid {
		t := lastSeen.Time.UTC()
		device.LastSeenAt = &t
	}
	if revokedAt.Valid {
		v := revokedAt.Time.UTC()
		device.RevokedAt = &v
	}
	device.CreatedAt = device.CreatedAt.UTC()
	device.UpdatedAt = device.UpdatedAt.UTC()
	return &device, nil
}

func (s *postgresStore) UpsertOverlayNode(ctx context.Context, node *OverlayNode) error {
	if node == nil {
		return errors.New("overlay node is required")
	}
	if strings.TrimSpace(node.GatewayMode) == "" {
		node.GatewayMode = "shadow"
	}
	if node.GatewayMode != "shadow" && node.GatewayMode != "apply" {
		return errors.New("overlay node gateway mode must be shadow or apply")
	}
	const query = `
		INSERT INTO overlay_nodes (
			id, network_id, name, role, region, wireguard_public_key,
			wireguard_address, endpoint_host, endpoint_port, transport_type,
			transport_security, transport_path, transport_mode, transport_uuid,
			gateway_mode, healthy, last_heartbeat, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, now())
		ON CONFLICT (id) DO UPDATE SET
			network_id = EXCLUDED.network_id,
			name = EXCLUDED.name,
			role = EXCLUDED.role,
			region = EXCLUDED.region,
			wireguard_public_key = EXCLUDED.wireguard_public_key,
			wireguard_address = EXCLUDED.wireguard_address,
			endpoint_host = EXCLUDED.endpoint_host,
			endpoint_port = EXCLUDED.endpoint_port,
			transport_type = EXCLUDED.transport_type,
			transport_security = EXCLUDED.transport_security,
			transport_path = EXCLUDED.transport_path,
			transport_mode = EXCLUDED.transport_mode,
			transport_uuid = EXCLUDED.transport_uuid,
			gateway_mode = EXCLUDED.gateway_mode,
			healthy = EXCLUDED.healthy,
			last_heartbeat = EXCLUDED.last_heartbeat,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(ctx, query,
		strings.TrimSpace(node.ID),
		strings.TrimSpace(node.NetworkID),
		strings.TrimSpace(node.Name),
		strings.TrimSpace(node.Role),
		strings.TrimSpace(node.Region),
		strings.TrimSpace(node.WireGuardPublicKey),
		strings.TrimSpace(node.WireGuardAddress),
		strings.TrimSpace(node.EndpointHost),
		node.EndpointPort,
		strings.TrimSpace(node.TransportType),
		strings.TrimSpace(node.TransportSecurity),
		strings.TrimSpace(node.TransportPath),
		strings.TrimSpace(node.TransportMode),
		strings.TrimSpace(node.TransportUUID),
		strings.TrimSpace(node.GatewayMode),
		node.Healthy,
		node.LastHeartbeat,
	).Scan(&node.CreatedAt, &node.UpdatedAt)
}

func (s *postgresStore) ListOverlayNodes(ctx context.Context, networkID string) ([]OverlayNode, error) {
	args := []any{}
	query := `
		SELECT id, network_id, name, role, region, wireguard_public_key,
		       wireguard_address, endpoint_host, endpoint_port, transport_type,
		       transport_security, transport_path, transport_mode, transport_uuid, gateway_mode, healthy,
		       created_at, updated_at, last_heartbeat
		FROM overlay_nodes`
	if strings.TrimSpace(networkID) != "" {
		query += " WHERE network_id = $1"
		args = append(args, strings.TrimSpace(networkID))
	}
	query += " ORDER BY id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]OverlayNode, 0)
	for rows.Next() {
		node, err := scanOverlayNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *node)
	}
	return result, rows.Err()
}

func scanOverlayNode(row rowScanner) (*OverlayNode, error) {
	var node OverlayNode
	var lastHeartbeat sql.NullTime
	err := row.Scan(
		&node.ID,
		&node.NetworkID,
		&node.Name,
		&node.Role,
		&node.Region,
		&node.WireGuardPublicKey,
		&node.WireGuardAddress,
		&node.EndpointHost,
		&node.EndpointPort,
		&node.TransportType,
		&node.TransportSecurity,
		&node.TransportPath,
		&node.TransportMode,
		&node.TransportUUID,
		&node.GatewayMode,
		&node.Healthy,
		&node.CreatedAt,
		&node.UpdatedAt,
		&lastHeartbeat,
	)
	if err != nil {
		return nil, err
	}
	if lastHeartbeat.Valid {
		t := lastHeartbeat.Time.UTC()
		node.LastHeartbeat = &t
	}
	node.CreatedAt = node.CreatedAt.UTC()
	node.UpdatedAt = node.UpdatedAt.UTC()
	return &node, nil
}

func (s *postgresStore) UpsertOverlayConfigAck(ctx context.Context, ack *OverlayConfigAck) error {
	if ack == nil {
		return errors.New("overlay config ack is required")
	}
	if ack.ReceivedAt.IsZero() {
		ack.ReceivedAt = time.Now().UTC()
	}
	const query = `
		INSERT INTO overlay_config_acks (
			device_id, user_uuid, network_id, revision, digest, applied_at, received_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_uuid, device_id) DO UPDATE SET
			network_id = EXCLUDED.network_id,
			revision = EXCLUDED.revision,
			digest = EXCLUDED.digest,
			applied_at = EXCLUDED.applied_at,
			received_at = EXCLUDED.received_at`
	_, err := s.db.ExecContext(ctx, query,
		strings.TrimSpace(ack.DeviceID),
		strings.TrimSpace(ack.UserID),
		strings.TrimSpace(ack.NetworkID),
		strings.TrimSpace(ack.Revision),
		strings.TrimSpace(ack.Digest),
		ack.AppliedAt.UTC(),
		ack.ReceivedAt.UTC(),
	)
	return err
}
