-- Admin-managed subscription validity is account metadata, not a billing
-- record. Dates are stored at UTC midnight and the end date is inclusive;
-- accounts applies the Free 5GB fallback after the end date has elapsed.
ALTER TABLE public.users
  ADD COLUMN IF NOT EXISTS subscription_valid_from TIMESTAMPTZ NULL,
  ADD COLUMN IF NOT EXISTS subscription_valid_until TIMESTAMPTZ NULL,
  ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ NULL,
  ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ NULL;

ALTER TABLE public.users
  DROP CONSTRAINT IF EXISTS users_subscription_validity_order_ck;

ALTER TABLE public.users
  ADD CONSTRAINT users_subscription_validity_order_ck
  CHECK (
    subscription_valid_from IS NULL
    OR subscription_valid_until IS NULL
    OR subscription_valid_until >= subscription_valid_from
  );
