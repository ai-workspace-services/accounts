-- Per-network XConnect Gateway frontend. Empty keeps the deployment-wide
-- default (XCONNECT_GATEWAY_XRAY_FRONTEND), so existing networks are unchanged.
BEGIN;

ALTER TABLE public.overlay_networks
  ADD COLUMN IF NOT EXISTS gateway_frontend TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS gateway_listen_socket TEXT NOT NULL DEFAULT '';

COMMIT;
