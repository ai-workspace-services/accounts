-- Never discard financial referral history as a schema-only rollback.
BEGIN;
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM public.referral_reward_events)
     OR EXISTS (SELECT 1 FROM public.referral_rewards)
     OR EXISTS (SELECT 1 FROM public.referrals)
     OR EXISTS (SELECT 1 FROM public.referral_settlements)
     OR EXISTS (SELECT 1 FROM public.referral_settlement_items)
     OR EXISTS (SELECT 1 FROM public.referral_payment_risks)
     OR EXISTS (SELECT 1 FROM public.subscription_extension_ledger)
     OR EXISTS (SELECT 1 FROM public.referral_codes) THEN
    RAISE EXCEPTION 'referral rollback refused: financial or attribution rows exist; export/reconcile data and use a reviewed forward migration';
  END IF;
  IF EXISTS (SELECT 1 FROM public.finance_invoices WHERE service_period_start IS NOT NULL OR service_period_end IS NOT NULL) THEN
    RAISE EXCEPTION 'referral rollback refused: verified subscription period evidence exists; preserve the evidence and use a reviewed forward migration';
  END IF;
END $$;

DROP TABLE IF EXISTS public.subscription_extension_ledger;
DROP TABLE IF EXISTS public.referral_settlement_items;
DROP TABLE IF EXISTS public.referral_settlements;
DROP TABLE IF EXISTS public.referral_payment_risks;
DROP TABLE IF EXISTS public.referral_reward_events;
DROP TABLE IF EXISTS public.referral_rewards;
DROP TABLE IF EXISTS public.referrals;
DROP TABLE IF EXISTS public.referral_codes;
ALTER TABLE public.finance_invoices DROP CONSTRAINT IF EXISTS finance_invoices_service_period_ck;
ALTER TABLE public.finance_invoices DROP COLUMN IF EXISTS service_period_start;
ALTER TABLE public.finance_invoices DROP COLUMN IF EXISTS service_period_end;
CREATE OR REPLACE FUNCTION public.reject_finance_fact_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only; % is not permitted', TG_TABLE_NAME, TG_OP;
  RETURN OLD;
END;
$$;
COMMIT;
