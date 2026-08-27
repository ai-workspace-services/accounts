-- XConnect-One invite enrollment. Only SHA-256 digests of join and enrollment
-- bearer secrets are persisted. Raw secrets exist only in the create/exchange
-- response process memory and must never be logged.
BEGIN;

CREATE TABLE IF NOT EXISTS public.overlay_join_tokens (
  id TEXT PRIMARY KEY,
  token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  network_id TEXT NOT NULL CHECK (btrim(network_id) <> ''),
  device_id TEXT,
  platform TEXT,
  remaining_uses INTEGER NOT NULL CHECK (remaining_uses IN (0, 1)),
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  last_exchanged_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT overlay_join_tokens_device_ck CHECK (device_id IS NULL OR btrim(device_id) <> ''),
  CONSTRAINT overlay_join_tokens_platform_ck CHECK (platform IS NULL OR platform IN ('darwin', 'windows', 'linux', 'ios', 'android')),
  CONSTRAINT overlay_join_tokens_expiry_ck CHECK (expires_at > created_at),
  CONSTRAINT overlay_join_tokens_scope_uk UNIQUE (id, user_uuid, network_id)
);

CREATE INDEX IF NOT EXISTS overlay_join_tokens_owner_idx
  ON public.overlay_join_tokens (user_uuid, created_at DESC);
CREATE INDEX IF NOT EXISTS overlay_join_tokens_expiry_idx
  ON public.overlay_join_tokens (expires_at) WHERE revoked_at IS NULL AND remaining_uses > 0;

CREATE TABLE IF NOT EXISTS public.overlay_enrollment_sessions (
  id TEXT PRIMARY KEY,
  token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  join_token_id TEXT NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  network_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  platform TEXT NOT NULL CHECK (platform IN ('darwin', 'windows', 'linux', 'ios', 'android')),
  wireguard_public_key TEXT NOT NULL CHECK (btrim(wireguard_public_key) <> ''),
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  CONSTRAINT overlay_enrollment_sessions_join_device_uk UNIQUE (join_token_id, device_id),
  CONSTRAINT overlay_enrollment_sessions_join_scope_fk FOREIGN KEY (join_token_id, user_uuid, network_id)
    REFERENCES public.overlay_join_tokens(id, user_uuid, network_id) ON DELETE CASCADE,
  CONSTRAINT overlay_enrollment_sessions_device_fk FOREIGN KEY (user_uuid, device_id)
    REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE,
  CONSTRAINT overlay_enrollment_sessions_expiry_ck CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS overlay_enrollment_sessions_expiry_idx
  ON public.overlay_enrollment_sessions (expires_at);

COMMIT;
