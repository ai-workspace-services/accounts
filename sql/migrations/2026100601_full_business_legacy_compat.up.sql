-- Additive compatibility for complete legacy Accounts data initialization.
-- Environment migration/checkpoint ledgers are never copied from PROD.
-- Existing user/device/node/ack rows and their primary keys remain intact.
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

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

CREATE INDEX IF NOT EXISTS idx_overlay_nodes_network ON public.overlay_nodes(network_id);

-- Legacy transport metadata is private; do not inherit Supabase client grants.
ALTER TABLE public.overlay_nodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.overlay_config_acks ENABLE ROW LEVEL SECURITY;
DO $$ DECLARE tab text; client text; BEGIN
 FOREACH tab IN ARRAY ARRAY['overlay_nodes','overlay_config_acks'] LOOP
  FOREACH client IN ARRAY ARRAY['anon','authenticated'] LOOP
   IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname=client) THEN
    EXECUTE format('REVOKE ALL PRIVILEGES ON public.%I FROM %I',tab,client);
   END IF;
  END LOOP;
 END LOOP;
END $$;
