-- P0-4: persist hashed, expiring, one-time MFA recovery codes.
-- Additive only: create the recovery-code table and indexes without changing
-- existing account, subscription, payment, refund, usage, or session records.
BEGIN;

CREATE TABLE IF NOT EXISTS public.mfa_recovery_codes (
  id UUID PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  batch_uuid UUID NOT NULL,
  code_hash TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  locked_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  consumed_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  CONSTRAINT mfa_recovery_codes_attempts_ck CHECK (failed_attempts >= 0),
  CONSTRAINT mfa_recovery_codes_hash_ck CHECK (length(code_hash) > 0),
  CONSTRAINT mfa_recovery_codes_terminal_state_ck
    CHECK (consumed_at IS NULL OR revoked_at IS NULL)
);

CREATE INDEX IF NOT EXISTS mfa_recovery_codes_active_user_idx
  ON public.mfa_recovery_codes (user_uuid, batch_uuid, created_at)
  WHERE consumed_at IS NULL AND revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS mfa_recovery_codes_expiry_idx
  ON public.mfa_recovery_codes (expires_at)
  WHERE consumed_at IS NULL AND revoked_at IS NULL;

ALTER TABLE public.mfa_recovery_codes ENABLE ROW LEVEL SECURITY;

COMMENT ON TABLE public.mfa_recovery_codes IS
  'One-time MFA recovery code hashes; plaintext values are returned only on issuance.';

COMMIT;
