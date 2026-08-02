-- Canonical account identity migration.
--
-- proxy_uuid remains in the schema for compatibility with older clients, but
-- it must not diverge from users.uuid. This migration is idempotent and safe
-- to run once during the UAT rollout (the service startup also applies the
-- same invariant for upgraded databases).
BEGIN;

UPDATE public.users
SET proxy_uuid = uuid
WHERE proxy_uuid IS DISTINCT FROM uuid;

ALTER TABLE public.users
  ALTER COLUMN proxy_uuid DROP DEFAULT;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'users_proxy_uuid_matches_uuid_ck'
      AND conrelid = 'public.users'::regclass
  ) THEN
    ALTER TABLE public.users
      ADD CONSTRAINT users_proxy_uuid_matches_uuid_ck
      CHECK (proxy_uuid = uuid);
  END IF;
END
$$;

COMMIT;
