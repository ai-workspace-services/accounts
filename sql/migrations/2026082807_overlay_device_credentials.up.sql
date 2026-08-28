BEGIN;
ALTER TABLE public.overlay_devices
  ADD CONSTRAINT overlay_devices_user_network_device_uk UNIQUE(user_uuid,network_id,id);
CREATE TABLE public.overlay_device_credentials(
  credential_id TEXT PRIMARY KEY CHECK(credential_id ~ '^xdcid_[0-9a-f]{32}$'),
  verifier_sha256 BYTEA NOT NULL UNIQUE CHECK(octet_length(verifier_sha256)=32),
  user_uuid UUID NOT NULL,
  network_id TEXT NOT NULL CHECK(btrim(network_id)<>''),
  device_id TEXT NOT NULL CHECK(btrim(device_id)<>''),
  status TEXT NOT NULL CHECK(status IN ('active','replaced','revoked')),
  scope JSONB NOT NULL CHECK(scope='["overlay:session:mint","overlay:credential:rotate","overlay:device:revoke"]'::JSONB),
  replaces_credential_id TEXT,
  replaced_by_credential_id TEXT,
  rotation_request_sha256 TEXT CHECK(rotation_request_sha256 IS NULL OR rotation_request_sha256 ~ '^[a-f0-9]{64}$'),
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT overlay_device_credentials_lifetime_ck CHECK(expires_at>issued_at AND expires_at<=issued_at+INTERVAL '31 days'),
  CONSTRAINT overlay_device_credentials_status_ck CHECK((status='revoked' AND revoked_at IS NOT NULL) OR (status<>'revoked' AND revoked_at IS NULL)),
  CONSTRAINT overlay_device_credentials_replacement_ck CHECK((replaces_credential_id IS NULL AND rotation_request_sha256 IS NULL) OR (replaces_credential_id IS NOT NULL AND rotation_request_sha256 IS NOT NULL)),
  CONSTRAINT overlay_device_credentials_device_fk FOREIGN KEY(user_uuid,network_id,device_id) REFERENCES public.overlay_devices(user_uuid,network_id,id) ON DELETE RESTRICT,
  CONSTRAINT overlay_device_credentials_binding_uk UNIQUE(credential_id,user_uuid,network_id,device_id),
  CONSTRAINT overlay_device_credentials_replaces_fk FOREIGN KEY(replaces_credential_id) REFERENCES public.overlay_device_credentials(credential_id) ON DELETE RESTRICT,
  CONSTRAINT overlay_device_credentials_replaced_by_fk FOREIGN KEY(replaced_by_credential_id) REFERENCES public.overlay_device_credentials(credential_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX overlay_device_credentials_active_device_uk ON public.overlay_device_credentials(user_uuid,network_id,device_id) WHERE status='active';
CREATE UNIQUE INDEX overlay_device_credentials_successor_uk ON public.overlay_device_credentials(replaces_credential_id) WHERE replaces_credential_id IS NOT NULL;
CREATE INDEX overlay_device_credentials_device_history_idx ON public.overlay_device_credentials(user_uuid,network_id,device_id,created_at);

CREATE TABLE public.overlay_device_sessions(
  session_id TEXT PRIMARY KEY CHECK(btrim(session_id)<>''),
  token_hash BYTEA NOT NULL UNIQUE CHECK(octet_length(token_hash)=32),
  credential_id TEXT NOT NULL,
  user_uuid UUID NOT NULL,
  network_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  scope JSONB NOT NULL CHECK(scope='["overlay:config:read","overlay:config:ack"]'::JSONB),
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  last_used_at TIMESTAMPTZ,
  CONSTRAINT overlay_device_sessions_lifetime_ck CHECK(expires_at>created_at AND expires_at<=created_at+INTERVAL '15 minutes'),
  CONSTRAINT overlay_device_sessions_binding_fk FOREIGN KEY(credential_id,user_uuid,network_id,device_id) REFERENCES public.overlay_device_credentials(credential_id,user_uuid,network_id,device_id) ON DELETE RESTRICT
);
CREATE INDEX overlay_device_sessions_expiry_idx ON public.overlay_device_sessions(expires_at);

CREATE TABLE public.overlay_device_revoke_receipts(
  user_uuid UUID NOT NULL,
  network_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[a-f0-9]{64}$'),
  client_nonce UUID NOT NULL,
  device_payload JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(user_uuid,network_id,device_id),
  CONSTRAINT overlay_device_revoke_receipts_binding_fk FOREIGN KEY(credential_id,user_uuid,network_id,device_id) REFERENCES public.overlay_device_credentials(credential_id,user_uuid,network_id,device_id) ON DELETE RESTRICT,
  CONSTRAINT overlay_device_revoke_receipts_no_secrets_ck CHECK(device_payload::text !~* '"(credential|verifier|private_key|refresh_token|vault_token)"[[:space:]]*:')
);
COMMIT;
