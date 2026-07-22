CREATE TABLE IF NOT EXISTS public.overlay_devices (
  id TEXT NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  network_id TEXT NOT NULL DEFAULT 'xworkmate-private',
  name TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT '',
  hostname TEXT NOT NULL DEFAULT '',
  wireguard_public_key TEXT NOT NULL,
  wireguard_address TEXT NOT NULL,
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, id)
);

CREATE TABLE IF NOT EXISTS public.overlay_nodes (
  id TEXT PRIMARY KEY,
  network_id TEXT NOT NULL DEFAULT 'xworkmate-private',
  name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'gateway',
  region TEXT NOT NULL DEFAULT '',
  wireguard_public_key TEXT NOT NULL,
  wireguard_address TEXT NOT NULL,
  endpoint_host TEXT NOT NULL,
  endpoint_port INTEGER NOT NULL DEFAULT 2443,
  transport_type TEXT NOT NULL DEFAULT 'vless-tls',
  transport_security TEXT NOT NULL DEFAULT 'tls',
  transport_path TEXT NOT NULL DEFAULT '',
  transport_mode TEXT NOT NULL DEFAULT '',
  transport_uuid TEXT NOT NULL DEFAULT '',
  healthy BOOLEAN NOT NULL DEFAULT FALSE,
  last_heartbeat TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE public.overlay_nodes
  ADD COLUMN IF NOT EXISTS transport_uuid TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS public.overlay_config_acks (
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  device_id TEXT NOT NULL,
  network_id TEXT NOT NULL DEFAULT 'xworkmate-private',
  revision TEXT NOT NULL,
  digest TEXT NOT NULL DEFAULT '',
  applied_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, device_id),
  FOREIGN KEY (user_uuid, device_id) REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_overlay_devices_network ON public.overlay_devices(network_id);
DROP INDEX IF EXISTS public.idx_overlay_devices_user_network_address;
CREATE UNIQUE INDEX IF NOT EXISTS idx_overlay_devices_network_address
  ON public.overlay_devices(network_id, wireguard_address);
CREATE INDEX IF NOT EXISTS idx_overlay_nodes_network ON public.overlay_nodes(network_id);
