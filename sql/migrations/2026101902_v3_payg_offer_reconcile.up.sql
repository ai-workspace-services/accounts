-- Make the complete approved PAYG preset catalog explicit on restored V3
-- databases. Preserve Stripe Price IDs and every financial/usage history row.
LOCK TABLE public.billing_plans IN SHARE ROW EXCLUSIVE MODE;

UPDATE public.billing_plans
SET active = false, updated_at = now()
WHERE plan_id IN ('PAYG-TOPUP-10', 'PAYG-TOPUP-500',
                  'PAYG-TOPUP-USD-5', 'PAYG-TOPUP-USD-10');

INSERT INTO public.billing_plans
  (plan_id, stripe_price_id, display_name, kind, included_quota_bytes,
   package_name, price_amount, price_currency, price_unit,
   price_multipliers, features, trial_days, active, sort_order)
VALUES
  ('PAYG-TOPUP-50', NULL, '余额充值 ¥50', 'paygo_topup', 0, 'payg',
   5000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb":1,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":50,"wallet_currency":"CNY","dunning":{"policy":"suspend_on_zero_balance","grace_hours":0}}'::jsonb,
   0, true, 21),
  ('PAYG-TOPUP-100', NULL, '余额充值 ¥100', 'paygo_topup', 0, 'payg',
   10000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb":1,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":100,"wallet_currency":"CNY","dunning":{"policy":"suspend_on_zero_balance","grace_hours":0}}'::jsonb,
   0, true, 22),
  ('PAYG-TOPUP-200', NULL, '余额充值 ¥200', 'paygo_topup', 0, 'payg',
   20000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb":1,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":200,"wallet_currency":"CNY","dunning":{"policy":"suspend_on_zero_balance","grace_hours":0}}'::jsonb,
   0, true, 23),
  ('PAYG-TOPUP-USD-20', NULL, 'Balance top-up $20', 'paygo_topup', 0, 'payg',
   2000, 'USD', 'once',
   '{"fast_lane_cny_per_gb":0,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":20,"wallet_currency":"USD"}'::jsonb,
   0, true, 26),
  ('PAYG-TOPUP-USD-50', NULL, 'Balance top-up $50', 'paygo_topup', 0, 'payg',
   5000, 'USD', 'once',
   '{"fast_lane_cny_per_gb":0,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":50,"wallet_currency":"USD"}'::jsonb,
   0, true, 27),
  ('PAYG-TOPUP-USD-100', NULL, 'Balance top-up $100', 'paygo_topup', 0, 'payg',
   10000, 'USD', 'once',
   '{"fast_lane_cny_per_gb":0,"vps_lane_cny_per_gb":0}'::jsonb,
   '{"topup_amount":100,"wallet_currency":"USD"}'::jsonb,
   0, true, 28)
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
