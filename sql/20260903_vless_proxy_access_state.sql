-- Operator-controlled VLESS access gate. It intentionally does not reuse
-- suspend_state, which is owned by the billing arrears workflow.
ALTER TABLE public.account_quota_states
  ADD COLUMN IF NOT EXISTS proxy_access_state TEXT NOT NULL DEFAULT 'active';
