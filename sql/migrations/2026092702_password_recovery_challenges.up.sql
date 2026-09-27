-- P0-2: persist one-time password recovery challenges and support atomic
-- password update + challenge consumption + session revocation.
-- Additive only: this migration creates a new table/indexes and never updates
-- existing users, subscriptions, payments, usage records, or sessions.
BEGIN;

CREATE TABLE IF NOT EXISTS public.password_recovery_challenges (
  id UUID PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  email_snapshot TEXT NOT NULL,
  challenge_kind TEXT NOT NULL,
  secret_hash TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  locked_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  consumed_at TIMESTAMPTZ,
  invalidated_at TIMESTAMPTZ,
  CONSTRAINT password_recovery_challenges_kind_ck
    CHECK (challenge_kind IN ('token', 'code')),
  CONSTRAINT password_recovery_challenges_attempts_ck
    CHECK (failed_attempts >= 0),
  CONSTRAINT password_recovery_challenges_hash_ck
    CHECK (length(secret_hash) > 0),
  CONSTRAINT password_recovery_challenges_terminal_state_ck
    CHECK (consumed_at IS NULL OR invalidated_at IS NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS password_recovery_active_user_kind_uk
  ON public.password_recovery_challenges (user_uuid, challenge_kind)
  WHERE consumed_at IS NULL AND invalidated_at IS NULL;

CREATE INDEX IF NOT EXISTS password_recovery_code_email_created_idx
  ON public.password_recovery_challenges (lower(email_snapshot), created_at DESC)
  WHERE challenge_kind = 'code' AND consumed_at IS NULL AND invalidated_at IS NULL;

CREATE INDEX IF NOT EXISTS password_recovery_expiry_idx
  ON public.password_recovery_challenges (expires_at)
  WHERE consumed_at IS NULL AND invalidated_at IS NULL;

ALTER TABLE public.password_recovery_challenges ENABLE ROW LEVEL SECURITY;

COMMENT ON TABLE public.password_recovery_challenges IS
  'Durable, expiring, one-time password recovery challenges; secrets are stored only as hashes.';

COMMIT;
