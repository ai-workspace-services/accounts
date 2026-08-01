-- Xray usage/billing minimal scope: billing period bounds for the current
-- quota grant, so usage/summary can answer "how much used this period" and
-- "when does it reset" without needing minute-bucket aggregation. Written
-- by entitlement sync on grant/reset (Accounts owns quota-grant fields);
-- billing-service only consumes. Nullable/idempotent: safe to re-run, and
-- existing rows simply start with no period recorded until next reset.
ALTER TABLE public.account_quota_states
  ADD COLUMN IF NOT EXISTS period_start TIMESTAMPTZ NULL;
ALTER TABLE public.account_quota_states
  ADD COLUMN IF NOT EXISTS period_end TIMESTAMPTZ NULL;
