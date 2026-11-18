-- Per-network XConnect Zero connection mode and non-secret runtime telemetry projection.
BEGIN;

ALTER TABLE public.overlay_networks
  ADD COLUMN IF NOT EXISTS connection_mode TEXT NOT NULL DEFAULT 'wg_vless_l3';

CREATE TABLE IF NOT EXISTS public.overlay_device_telemetry (
  network_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'one',
  healthy BOOLEAN NOT NULL DEFAULT FALSE,
  connection_mode TEXT NOT NULL DEFAULT '',
  applied_config_id TEXT NOT NULL DEFAULT '',
  applied_generation BIGINT NOT NULL DEFAULT 0,
  active_path TEXT NOT NULL DEFAULT '',
  endpoint TEXT NOT NULL DEFAULT '',
  rtt_ms BIGINT NOT NULL DEFAULT 0,
  rx_bytes BIGINT NOT NULL DEFAULT 0,
  tx_bytes BIGINT NOT NULL DEFAULT 0,
  internet_rx_bytes BIGINT NOT NULL DEFAULT 0,
  internet_tx_bytes BIGINT NOT NULL DEFAULT 0,
  private_rx_bytes BIGINT NOT NULL DEFAULT 0,
  private_tx_bytes BIGINT NOT NULL DEFAULT 0,
  latest_handshake_at TIMESTAMPTZ,
  peers_json TEXT NOT NULL DEFAULT '[]',
  top_apps_json TEXT NOT NULL DEFAULT '[]',
  sampled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (network_id, device_id)
);

CREATE INDEX IF NOT EXISTS idx_overlay_device_telemetry_role
  ON public.overlay_device_telemetry (role);

COMMIT;
