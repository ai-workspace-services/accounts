-- Publish the revised V3 catalog without rewriting historical Stripe prices,
-- subscriptions, invoices, payments, refunds, wallets, or usage ledgers.
LOCK TABLE public.billing_plans IN SHARE ROW EXCLUSIVE MODE;

-- Keep the old IDs, prices, and rights intact for existing subscriptions and
-- historical invoice reconciliation; only prevent new checkout from using them.
UPDATE public.billing_plans
SET display_name = CASE plan_id
      WHEN 'PRO-MONTHLY' THEN 'Pro 月付（历史方案）'
      WHEN 'PRO-YEARLY' THEN 'Pro 年付（历史方案）'
    END,
    active = false, updated_at = now()
WHERE plan_id IN ('PRO-MONTHLY', 'PRO-YEARLY');

UPDATE public.billing_plans
SET price_amount = 2000, price_currency = 'CNY', price_unit = 'month',
    updated_at = now()
WHERE plan_id = 'PLUS';

INSERT INTO public.billing_plans
  (plan_id, stripe_price_id, display_name, kind, included_quota_bytes,
   package_name, price_amount, price_currency, price_unit,
   price_multipliers, features, trial_days, active, sort_order)
VALUES
  ('PRO-MONTHLY-V2', NULL, 'Pro 月付', 'subscription', 53687091200,
   'pro', 5000, 'CNY', 'month',
   '{"fast_lane_cny_per_gb":1,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"sla":"standard","session_persistence":true,"resource_cards":{"create":true,"pricing":"list_price_plus_managed_fee","managed_fee_rate":0.20},"fast_lane":{"mode":"quota"},"overage":{"policy":"charge","cny_per_gb":1.0},"dunning":{"policy":"grace_then_suspend","grace_days":14},"quota_cycle":"natural_month"}'::jsonb,
   0, true, 30),
  ('PRO-YEARLY-V2', NULL, 'Pro 年付', 'subscription', 9223372036854775807,
   'pro', 50000, 'CNY', 'year',
   '{"fast_lane_cny_per_gb":0,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"sla":"standard","session_persistence":true,"resource_cards":{"create":true,"pricing":"list_price_plus_managed_fee","managed_fee_rate":0.20},"fast_lane":{"mode":"unlimited"},"overage":{"policy":"none"},"dunning":{"policy":"grace_then_suspend","grace_days":14},"quota_cycle":"none"}'::jsonb,
   0, true, 40)
ON CONFLICT (plan_id) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  kind = EXCLUDED.kind,
  included_quota_bytes = EXCLUDED.included_quota_bytes,
  package_name = EXCLUDED.package_name,
  price_amount = EXCLUDED.price_amount,
  price_currency = EXCLUDED.price_currency,
  price_unit = EXCLUDED.price_unit,
  price_multipliers = EXCLUDED.price_multipliers,
  features = EXCLUDED.features,
  active = EXCLUDED.active,
  sort_order = EXCLUDED.sort_order,
  updated_at = now();

-- Retire previously sold denominations but retain their rows/Stripe IDs for
-- old payment reconciliation. New custom amounts are ephemeral Checkout
-- price_data and are validated by Accounts; only these values are presets.
UPDATE public.billing_plans
SET active = false, updated_at = now()
WHERE plan_id IN ('PAYG-TOPUP-10', 'PAYG-TOPUP-500',
                  'PAYG-TOPUP-USD-5', 'PAYG-TOPUP-USD-10');

INSERT INTO public.billing_plans
  (plan_id, stripe_price_id, display_name, kind, included_quota_bytes,
   package_name, price_amount, price_currency, price_unit,
   price_multipliers, features, trial_days, active, sort_order)
VALUES
  ('PAYG-TOPUP-200', NULL, '余额充值 ¥200', 'paygo_topup', 0, 'payg',
   20000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb":1,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":200,"wallet_currency":"CNY","dunning":{"policy":"suspend_on_zero_balance","grace_hours":0}}'::jsonb,
   0, true, 23),
  ('PAYG-TOPUP-USD-50', NULL, 'Balance top-up $50', 'paygo_topup', 0, 'payg',
   5000, 'USD', 'once',
   '{"fast_lane_cny_per_gb":0,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":50,"wallet_currency":"USD"}'::jsonb,
   0, true, 27)
ON CONFLICT (plan_id) DO NOTHING;

