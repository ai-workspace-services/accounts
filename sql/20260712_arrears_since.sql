-- Stripe billing P1.5: track when an account entered its current arrears
-- episode so billing-service can escalate to suspend_state='suspended' once
-- the configured grace threshold elapses. Nullable/idempotent: safe to
-- re-run, and existing rows simply start with no episode in flight.
ALTER TABLE public.account_quota_states
  ADD COLUMN IF NOT EXISTS arrears_since TIMESTAMPTZ NULL;
