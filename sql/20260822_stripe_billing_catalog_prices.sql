-- Stripe billing catalog price snapshot fields.
-- Stripe remains the source of truth for charging; these fields are the
-- audited public catalog values consumed by /api/billing/plans.

ALTER TABLE public.billing_plans
  ADD COLUMN IF NOT EXISTS price_amount BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS price_currency TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS price_unit TEXT NOT NULL DEFAULT '';
