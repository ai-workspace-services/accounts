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
	const query = `
		INSERT INTO overlay_devices (
			id, user_uuid, network_id, name, platform, hostname,
			wireguard_public_key, wireguard_address, last_seen_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (user_uuid, id) DO UPDATE SET
			network_id = EXCLUDED.network_id,
			name = EXCLUDED.name,
			platform = EXCLUDED.platform,
			hostname = EXCLUDED.hostname,
			wireguard_public_key = EXCLUDED.wireguard_public_key,
			wireguard_address = EXCLUDED.wireguard_address,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = now()
		RETURNING created_at, updated_at`

	err := s.db.QueryRowContext(ctx, query,
		strings.TrimSpace(device.ID),
		strings.TrimSpace(device.UserID),
		strings.TrimSpace(device.NetworkID),
		strings.TrimSpace(device.Name),
		strings.TrimSpace(device.Platform),
		strings.TrimSpace(device.Hostname),
		strings.TrimSpace(device.WireGuardPublicKey),
		strings.TrimSpace(device.WireGuardAddress),
		device.LastSeenAt,
	).Scan(&device.CreatedAt, &device.UpdatedAt)
	if err != nil {
		return err
	}
	device.ID = strings.TrimSpace(device.ID)
	device.UserID = strings.TrimSpace(device.UserID)
	return nil
}

func (s *postgresStore) GetOverlayDevice(ctx context.Context, userID, deviceID string) (*OverlayDevice, error) {
	const query = `
		SELECT id, user_uuid, network_id, name, platform, hostname,
		       wireguard_public_key, wireguard_address, created_at, updated_at, last_seen_at
		FROM overlay_devices
		WHERE user_uuid = $1 AND id = $2`
	row := s.db.QueryRowContext(ctx, query, strings.TrimSpace(userID), strings.TrimSpace(deviceID))
	return scanOverlayDevice(row)
}

func (s *postgresStore) ListOverlayDevicesByUser(ctx context.Context, userID string) ([]OverlayDevice, error) {
	const query = `
		SELECT id, user_uuid, network_id, name, platform, hostname,
		       wireguard_public_key, wireguard_address, created_at, updated_at, last_seen_at
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
		       wireguard_public_key, wireguard_address, created_at, updated_at, last_seen_at
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
	var lastSeen sql.NullTime
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
	device.CreatedAt = device.CreatedAt.UTC()
	device.UpdatedAt = device.UpdatedAt.UTC()
	return &device, nil
}

func (s *postgresStore) UpsertOverlayNode(ctx context.Context, node *OverlayNode) error {
	if node == nil {
		return errors.New("overlay node is required")
	}
	const query = `
		INSERT INTO overlay_nodes (
			id, network_id, name, role, region, wireguard_public_key,
			wireguard_address, endpoint_host, endpoint_port, transport_type,
			transport_security, transport_path, transport_mode, transport_uuid,
			healthy, last_heartbeat, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now())
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
		node.Healthy,
		node.LastHeartbeat,
	).Scan(&node.CreatedAt, &node.UpdatedAt)
}

func (s *postgresStore) ListOverlayNodes(ctx context.Context, networkID string) ([]OverlayNode, error) {
	args := []any{}
	query := `
		SELECT id, network_id, name, role, region, wireguard_public_key,
		       wireguard_address, endpoint_host, endpoint_port, transport_type,
		       transport_security, transport_path, transport_mode, transport_uuid, healthy,
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
