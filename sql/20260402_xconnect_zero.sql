-- XConnect Zero v1 additive schema.
-- Runtime secrets are represented only by SHA-256 digests. The client
-- WireGuard private key is generated and retained by XConnect-One.
CREATE TABLE IF NOT EXISTS overlay_networks (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL DEFAULT '',
  cidr TEXT NOT NULL,
  gateway_id TEXT NOT NULL,
  gateway_wireguard_key TEXT NOT NULL,
  gateway_wireguard_address TEXT NOT NULL DEFAULT '',
  gateway_endpoint_host TEXT NOT NULL,
  gateway_endpoint_port INTEGER NOT NULL,
  transport_server_name TEXT NOT NULL,
  transport_port INTEGER NOT NULL,
  transport_auth_id TEXT NOT NULL,
  owner_user_id TEXT NOT NULL DEFAULT '',
  config_generation BIGINT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS overlay_invites (
  id TEXT PRIMARY KEY,
  network_id TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  device_id TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'one',
  expires_at TIMESTAMPTZ NOT NULL,
  remaining_uses INTEGER NOT NULL DEFAULT 1,
  consumed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS overlay_devices (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL DEFAULT '',
  network_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'one',
  name TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL,
  hostname TEXT NOT NULL DEFAULT '',
  wireguard_public_key TEXT NOT NULL,
  wireguard_address TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (network_id, wireguard_address)
);

CREATE TABLE IF NOT EXISTS overlay_device_credentials (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  credential_id TEXT NOT NULL UNIQUE,
  token_hash TEXT NOT NULL UNIQUE,
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS overlay_enrollment_sessions (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  consumed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS overlay_signed_config_acks (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  network_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  config_id TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL,
  UNIQUE (device_id, generation)
);

CREATE INDEX IF NOT EXISTS overlay_invites_network_idx ON overlay_invites (network_id);
CREATE INDEX IF NOT EXISTS overlay_devices_network_idx ON overlay_devices (network_id);
CREATE INDEX IF NOT EXISTS overlay_credentials_device_idx ON overlay_device_credentials (device_id);
CREATE INDEX IF NOT EXISTS overlay_enrollments_device_idx ON overlay_enrollment_sessions (device_id);
CREATE INDEX IF NOT EXISTS overlay_acks_network_idx ON overlay_signed_config_acks (network_id);
