-- Online upgrade only: this migration does not update users.uuid, username,
-- email, password, MFA columns, or any existing Xray client id.
BEGIN;

ALTER TABLE public.users
  DROP CONSTRAINT IF EXISTS users_proxy_uuid_matches_uuid_ck;
DROP INDEX IF EXISTS public.users_single_root_role_uk;

CREATE TABLE IF NOT EXISTS public.bridge_credentials (
  credential_uuid UUID PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  token_hash TEXT,
  token_prefix TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
  source TEXT NOT NULL DEFAULT 'generated' CHECK (source IN ('generated', 'xray-import')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ,
  CONSTRAINT bridge_credentials_token_hash_ck CHECK (
    (status = 'active' AND revoked_at IS NULL) OR status = 'revoked'
  )
);

CREATE INDEX IF NOT EXISTS bridge_credentials_user_tenant_idx
  ON public.bridge_credentials (user_uuid, tenant_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS bridge_credentials_active_user_tenant_uk
  ON public.bridge_credentials (user_uuid, tenant_id) WHERE status = 'active';

COMMIT;
