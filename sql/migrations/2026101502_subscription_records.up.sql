ALTER TABLE public.users ADD COLUMN IF NOT EXISTS subscription_valid_from TIMESTAMPTZ;
ALTER TABLE public.users ADD COLUMN IF NOT EXISTS subscription_valid_until TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS public.subscription_records (
  user_uuid UUID PRIMARY KEY REFERENCES public.users(uuid) ON DELETE CASCADE,
  subscription_type TEXT NOT NULL DEFAULT '',
  monthly_quota_bytes BIGINT NOT NULL DEFAULT 0 CHECK (monthly_quota_bytes >= 0),
  annual_quota_bytes BIGINT NOT NULL DEFAULT 0 CHECK (annual_quota_bytes >= 0),
  fee_minor BIGINT NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
  payment_status TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  valid_from TIMESTAMPTZ,
  valid_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT subscription_records_validity_ck CHECK (valid_until IS NULL OR valid_from IS NULL OR valid_until >= valid_from)
);

CREATE INDEX IF NOT EXISTS subscription_records_validity_idx
  ON public.subscription_records (valid_until);
