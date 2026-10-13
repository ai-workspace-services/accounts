-- Referral campaign facts are additive and isolated from existing payment
-- records. Stripe invoice/payment/refund facts remain in finance_* tables.
BEGIN;

ALTER TABLE public.finance_invoices
  ADD COLUMN IF NOT EXISTS service_period_start TIMESTAMPTZ NULL,
  ADD COLUMN IF NOT EXISTS service_period_end TIMESTAMPTZ NULL;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='finance_invoices_service_period_ck') THEN
    ALTER TABLE public.finance_invoices ADD CONSTRAINT finance_invoices_service_period_ck
      CHECK ((service_period_start IS NULL) = (service_period_end IS NULL)
        AND (service_period_start IS NULL OR service_period_start < service_period_end));
  END IF;
END $$;
CREATE OR REPLACE FUNCTION public.reject_finance_fact_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_TABLE_NAME = 'finance_invoices'
     AND TG_OP = 'UPDATE'
     AND OLD.service_period_start IS NULL
     AND OLD.service_period_end IS NULL
     AND NEW.service_period_start IS NOT NULL
     AND NEW.service_period_end IS NOT NULL
     AND (to_jsonb(NEW) - 'service_period_start' - 'service_period_end') = (to_jsonb(OLD) - 'service_period_start' - 'service_period_end') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION '% is append-only; % is not permitted', TG_TABLE_NAME, TG_OP;
  RETURN OLD;
END;
$$;

CREATE TABLE IF NOT EXISTS public.referral_codes (
  user_uuid UUID PRIMARY KEY REFERENCES public.users(uuid) ON DELETE RESTRICT,
  code TEXT NOT NULL UNIQUE CHECK (code ~ '^[A-Z0-9]{8,16}$'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.referrals (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  referrer_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  invitee_uuid UUID NOT NULL UNIQUE REFERENCES public.users(uuid) ON DELETE RESTRICT,
  referral_code TEXT NOT NULL REFERENCES public.referral_codes(code) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT referrals_no_self_ck CHECK (referrer_uuid <> invitee_uuid)
);
CREATE INDEX IF NOT EXISTS referrals_referrer_created_idx
  ON public.referrals (referrer_uuid, created_at DESC);

CREATE TABLE IF NOT EXISTS public.referral_rewards (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  referral_id UUID NOT NULL UNIQUE REFERENCES public.referrals(id) ON DELETE RESTRICT,
  provider_invoice_id TEXT NOT NULL UNIQUE,
  referrer_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  invitee_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  plan_id TEXT NOT NULL,
  product_slug TEXT NOT NULL CHECK (product_slug = 'xconnect'),
  paid_minor BIGINT NOT NULL CHECK (paid_minor > 0),
  reward_minor BIGINT NOT NULL CHECK (reward_minor >= 0),
  currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  paid_at TIMESTAMPTZ NOT NULL,
  eligible_at TIMESTAMPTZ NOT NULL,
  annual_days INTEGER NOT NULL DEFAULT 0 CHECK (annual_days IN (0, 30)),
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'eligible', 'settled', 'reversed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS referral_rewards_referrer_eligible_idx
  ON public.referral_rewards (referrer_uuid, currency, eligible_at)
  WHERE status IN ('pending', 'eligible');

-- Risk can arrive before invoice.paid and before the local finance invoice is
-- materialized. Keep the provider invoice tombstone so replayed paid events
-- cannot create a reward after a refund or dispute.
CREATE TABLE IF NOT EXISTS public.referral_payment_risks (
  source_key TEXT PRIMARY KEY,
  provider_invoice_id TEXT NOT NULL,
  risk_type TEXT NOT NULL CHECK (risk_type IN ('refund', 'dispute')),
  reason TEXT NOT NULL DEFAULT '',
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS referral_payment_risks_invoice_idx
  ON public.referral_payment_risks (provider_invoice_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS public.referral_reward_events (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  reward_id UUID NOT NULL REFERENCES public.referral_rewards(id) ON DELETE RESTRICT,
  source_key TEXT NOT NULL UNIQUE,
  event_type TEXT NOT NULL CHECK (event_type IN ('award', 'reversal')),
  amount_delta_minor BIGINT NOT NULL CHECK (amount_delta_minor <> 0),
  reason TEXT NOT NULL DEFAULT '',
  actor_uuid UUID NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT referral_reward_events_reward_type_uk UNIQUE (reward_id, event_type)
);

CREATE TABLE IF NOT EXISTS public.referral_settlements (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  referrer_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
  status TEXT NOT NULL DEFAULT 'pending_review' CHECK (status IN ('pending_review', 'paid', 'rejected')),
  payout_reference TEXT NOT NULL DEFAULT '',
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ NULL,
  completed_by UUID NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS referral_settlements_owner_idx
  ON public.referral_settlements (referrer_uuid, requested_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS referral_settlements_payout_ref_uk
  ON public.referral_settlements (payout_reference)
  WHERE status='paid' AND payout_reference <> '';

CREATE TABLE IF NOT EXISTS public.referral_settlement_items (
  settlement_id UUID NOT NULL REFERENCES public.referral_settlements(id) ON DELETE RESTRICT,
  reward_id UUID NOT NULL REFERENCES public.referral_rewards(id) ON DELETE RESTRICT,
  amount_minor BIGINT NOT NULL CHECK (amount_minor <> 0),
  PRIMARY KEY (settlement_id, reward_id)
);

CREATE TABLE IF NOT EXISTS public.subscription_extension_ledger (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  reward_id UUID NOT NULL UNIQUE REFERENCES public.referral_rewards(id) ON DELETE RESTRICT,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  subscription_uuid UUID NOT NULL REFERENCES public.subscriptions(uuid) ON DELETE RESTRICT,
  days INTEGER NOT NULL CHECK (days = 30),
  effective_from TIMESTAMPTZ NOT NULL,
  effective_until TIMESTAMPTZ NOT NULL CHECK (effective_until > effective_from),
  status TEXT NOT NULL DEFAULT 'applied' CHECK (status IN ('applied', 'reversed')),
  reversal_source_key TEXT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscription_extension_owner_idx
  ON public.subscription_extension_ledger (user_uuid, created_at DESC);

COMMIT;
