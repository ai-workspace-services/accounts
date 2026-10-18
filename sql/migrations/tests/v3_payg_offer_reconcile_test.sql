\set ON_ERROR_STOP on
-- Run in an empty disposable database. This intentionally starts with a
-- partial/legacy catalog and proves the migration converges without replacing
-- existing Stripe Price IDs.
CREATE TABLE public.billing_plans (
  plan_id TEXT PRIMARY KEY,
  stripe_price_id TEXT UNIQUE,
  display_name TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'subscription',
  included_quota_bytes BIGINT NOT NULL DEFAULT 0,
  package_name TEXT NOT NULL DEFAULT 'default',
  price_amount BIGINT NOT NULL DEFAULT 0,
  price_currency TEXT NOT NULL DEFAULT '',
  price_unit TEXT NOT NULL DEFAULT '',
  price_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb,
  features JSONB NOT NULL DEFAULT '{}'::jsonb,
  trial_days INTEGER NOT NULL DEFAULT 0,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO public.billing_plans(plan_id, stripe_price_id, display_name, kind, package_name,
 price_amount, price_currency, price_unit, active)
VALUES ('PAYG-TOPUP-50','existing-price-reference','old name','paygo_topup','payg',1,'CNY','once',false),
       ('PAYG-TOPUP-10','retired-price-reference','old tier','paygo_topup','payg',1000,'CNY','once',true),
       ('PAYG-TOPUP-USD-10','retired-usd-reference','old tier','paygo_topup','payg',1000,'USD','once',true);

BEGIN;
\ir ../2026101902_v3_payg_offer_reconcile.up.sql
COMMIT;
BEGIN;
\ir ../2026101902_v3_payg_offer_reconcile.up.sql
COMMIT;

DO $$
DECLARE active_count INTEGER;
BEGIN
  SELECT count(*) INTO active_count FROM public.billing_plans
  WHERE active AND plan_id IN ('PAYG-TOPUP-50','PAYG-TOPUP-100','PAYG-TOPUP-200',
                               'PAYG-TOPUP-USD-20','PAYG-TOPUP-USD-50','PAYG-TOPUP-USD-100');
  IF active_count <> 6 THEN RAISE EXCEPTION 'expected all six fixed PAYG offers, got %', active_count; END IF;
  IF EXISTS (
    SELECT 1 FROM (VALUES
      ('PAYG-TOPUP-50',5000::bigint,'CNY','50','CNY'),
      ('PAYG-TOPUP-100',10000::bigint,'CNY','100','CNY'),
      ('PAYG-TOPUP-200',20000::bigint,'CNY','200','CNY'),
      ('PAYG-TOPUP-USD-20',2000::bigint,'USD','20','USD'),
      ('PAYG-TOPUP-USD-50',5000::bigint,'USD','50','USD'),
      ('PAYG-TOPUP-USD-100',10000::bigint,'USD','100','USD')
    ) AS expected(plan_id, amount, currency, topup_amount, wallet_currency)
    LEFT JOIN public.billing_plans p USING (plan_id)
    WHERE p.plan_id IS NULL OR NOT p.active OR p.kind <> 'paygo_topup'
       OR p.price_amount <> expected.amount OR p.price_currency <> expected.currency
       OR p.price_unit <> 'once' OR p.features->>'topup_amount' <> expected.topup_amount
       OR p.features->>'wallet_currency' <> expected.wallet_currency
  ) THEN RAISE EXCEPTION 'one or more fixed PAYG offers do not match the approved catalog'; END IF;
  IF (SELECT stripe_price_id FROM public.billing_plans WHERE plan_id='PAYG-TOPUP-50') <> 'existing-price-reference'
  THEN RAISE EXCEPTION 'migration overwrote an existing Stripe price mapping'; END IF;
  IF EXISTS (SELECT 1 FROM public.billing_plans WHERE plan_id IN
      ('PAYG-TOPUP-10','PAYG-TOPUP-500','PAYG-TOPUP-USD-5','PAYG-TOPUP-USD-10') AND active)
  THEN RAISE EXCEPTION 'retired denominations remain active'; END IF;
END $$;
