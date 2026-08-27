-- Durable XConnect-One SignedConfig projection state. The signed payload may
-- contain opaque transport auth_id. Protect the database volume and restrict
-- SELECT on these tables to the account service role. No signing private key is
-- stored here.
BEGIN;

CREATE TABLE IF NOT EXISTS public.overlay_signed_configs (
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  device_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  config_id TEXT NOT NULL,
  network_id TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  signing_key_id TEXT NOT NULL,
  signed_payload JSONB NOT NULL,
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, device_id, generation),
  CONSTRAINT overlay_signed_configs_config_uk UNIQUE (user_uuid, device_id, config_id),
  CONSTRAINT overlay_signed_configs_ack_target_uk UNIQUE (user_uuid, device_id, generation, config_id),
  CONSTRAINT overlay_signed_configs_device_fk FOREIGN KEY (user_uuid, device_id)
    REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE,
  CONSTRAINT overlay_signed_configs_xray_ck CHECK (signed_payload->>'proxy_core' = 'xray'),
  CONSTRAINT overlay_signed_configs_envelope_ck CHECK (
    signed_payload->>'config_id' = config_id
    AND signed_payload->>'network_id' = network_id
    AND signed_payload->>'device_id' = device_id
    AND (signed_payload->>'generation')::bigint = generation
    AND signed_payload->'signature'->>'key_id' = signing_key_id
  ),
  CONSTRAINT overlay_signed_configs_lifetime_ck CHECK (expires_at > issued_at),
  CONSTRAINT overlay_signed_configs_source_ck CHECK (btrim(source_revision) <> ''),
  CONSTRAINT overlay_signed_configs_no_secret_keys_ck CHECK (
    signed_payload::text !~* '"(private_key|refresh_token|vault_token)"[[:space:]]*:'
  )
);

CREATE INDEX IF NOT EXISTS overlay_signed_configs_latest_idx
  ON public.overlay_signed_configs (user_uuid, device_id, generation DESC);

CREATE TABLE IF NOT EXISTS public.overlay_signed_config_acks (
  user_uuid UUID NOT NULL,
  device_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  config_id TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, device_id, generation),
  CONSTRAINT overlay_signed_config_acks_config_fk
    FOREIGN KEY (user_uuid, device_id, generation, config_id)
    REFERENCES public.overlay_signed_configs(user_uuid, device_id, generation, config_id) ON DELETE CASCADE
);

COMMIT;
