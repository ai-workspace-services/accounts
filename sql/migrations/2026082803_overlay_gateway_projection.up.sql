-- XConnect-One Gateway Agent control plane. Bearer credentials are hash-only;
-- signed snapshots contain public projection data and credential references.
BEGIN;

CREATE TABLE public.overlay_node_credentials (
  id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL REFERENCES public.overlay_nodes(id) ON DELETE CASCADE,
  token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT overlay_node_credentials_expiry_ck CHECK (expires_at>created_at)
);

CREATE INDEX overlay_node_credentials_active_idx ON public.overlay_node_credentials(node_id,expires_at) WHERE revoked_at IS NULL;

CREATE UNIQUE INDEX overlay_devices_network_device_id_uk ON public.overlay_devices(network_id,id);
CREATE UNIQUE INDEX overlay_devices_network_public_key_uk ON public.overlay_devices(network_id,wireguard_public_key);
CREATE UNIQUE INDEX IF NOT EXISTS idx_overlay_devices_network_address ON public.overlay_devices(network_id,wireguard_address);

CREATE TABLE public.overlay_gateway_node_status (
  node_id TEXT PRIMARY KEY REFERENCES public.overlay_nodes(id) ON DELETE CASCADE,
  agent_version TEXT NOT NULL CHECK (btrim(agent_version)<>''),
  mode TEXT NOT NULL CHECK (mode='shadow'),
  proxy_core TEXT NOT NULL CHECK (proxy_core='xray'),
  observed_generation BIGINT NOT NULL CHECK (observed_generation>=0),
  applied_generation BIGINT NOT NULL CHECK (applied_generation=0),
  received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE public.overlay_gateway_snapshots (
  node_id TEXT NOT NULL REFERENCES public.overlay_nodes(id) ON DELETE CASCADE,
  snapshot_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation>0),
  expected_previous_generation BIGINT NOT NULL CHECK (expected_previous_generation>=0 AND generation>expected_previous_generation),
  source_revision TEXT NOT NULL CHECK (btrim(source_revision)<>''),
  signing_key_id TEXT NOT NULL CHECK (btrim(signing_key_id)<>''),
  signed_payload JSONB NOT NULL,
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (node_id,generation),
  CONSTRAINT overlay_gateway_snapshots_id_uk UNIQUE (node_id,snapshot_id),
  CONSTRAINT overlay_gateway_snapshots_source_uk UNIQUE (node_id,source_revision),
  CONSTRAINT overlay_gateway_snapshots_expiry_ck CHECK (expires_at>issued_at),
  CONSTRAINT overlay_gateway_snapshots_xray_ck CHECK (signed_payload->>'proxy_core'='xray'),
  CONSTRAINT overlay_gateway_snapshots_transport_ck CHECK (signed_payload#>>'{relay,transport}'='vless-tls-xudp'),
  CONSTRAINT overlay_gateway_snapshots_secret_ck CHECK (signed_payload::text !~* '"(private_key|auth_id|refresh_token|vault_token|transport_uuid|bearer_token)"[[:space:]]*:')
);

CREATE TABLE public.overlay_gateway_apply_results (
  node_id TEXT NOT NULL,
  snapshot_id TEXT NOT NULL,
  observed_generation BIGINT NOT NULL CHECK (observed_generation>0),
  applied_generation BIGINT NOT NULL CHECK (applied_generation=0),
  runtime_applied BOOLEAN NOT NULL CHECK (runtime_applied=FALSE),
  result TEXT NOT NULL CHECK (result IN ('shadow_validated','shadow_validated_wg_unavailable','shadow_rejected')),
  diff JSONB NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (node_id,snapshot_id),
  CONSTRAINT overlay_gateway_apply_results_snapshot_fk FOREIGN KEY (node_id,snapshot_id)
    REFERENCES public.overlay_gateway_snapshots(node_id,snapshot_id) ON DELETE CASCADE
);

CREATE TABLE public.overlay_device_projection_metadata (
  user_uuid UUID NOT NULL,
  device_id TEXT NOT NULL,
  tags JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(tags)='array'),
  attachments JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(attachments)='array'),
  source_kind TEXT NOT NULL,
  source_variable TEXT NOT NULL,
  baseline_sha256 TEXT NOT NULL CHECK (baseline_sha256~'^[a-f0-9]{64}$'),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid,device_id),
  CONSTRAINT overlay_device_projection_metadata_device_fk FOREIGN KEY (user_uuid,device_id)
    REFERENCES public.overlay_devices(user_uuid,id) ON DELETE CASCADE
);

CREATE TABLE public.overlay_static_import_receipts (
  import_id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key~'^sha256-[a-f0-9]{64}$'),
  body_sha256 TEXT NOT NULL CHECK (body_sha256~'^[a-f0-9]{64}$'),
  owner_user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  network_id TEXT NOT NULL CHECK (btrim(network_id)<>''),
  source_kind TEXT NOT NULL,
  source_variable TEXT NOT NULL,
  baseline_sha256 TEXT NOT NULL CHECK (baseline_sha256~'^[a-f0-9]{64}$'),
  device_count INTEGER NOT NULL CHECK (device_count>0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
