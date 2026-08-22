-- 套餐目录种子数据（幂等，可反复执行）。
--
-- 对应 docs/roadmap/feature-subscription-billing-operations/01-plan-catalog.md
-- 的四档设计 + 存量用户迁移用的过渡档（07-existing-user-migration.md）。
--
-- 用法：
--   docker exec -i web-saas-postgresql psql -U postgres -d account -v ON_ERROR_STOP=1 \
--     -f - < scripts/seed-billing-plans.sql
--
-- 几个必须遵守的约束（都在 api/billing_plans.go 与表定义里硬校验）：
--
--   1. kind 只接受 trial / subscription / paygo_topup 三个值。custom 档和
--      LEGACY-GRANDFATHERED 都用 subscription，靠 package_name + features
--      区分语义 —— 不要试图新增枚举值。
--
--   2. stripe_price_id 有 UNIQUE 约束，且"无 Stripe 价格"必须存 NULL 而**不是**
--      空串：UNIQUE 允许多个 NULL，但两条空串会直接撞约束。这里所有还没有真实
--      price id 的档位一律写 NULL，等 scripts/stripe-sync-catalog.sh 跑完再用
--      admin 接口回填真实的 price_xxx。
--
--   3. included_quota_bytes 是"周期内含高速流量字节数"。free 档写 0 不代表没有
--      高速流量，而是表示"不由字节配额管辖"——它由 features.fast_lane 的时间
--      窗口管辖（见 02-metering-and-entitlements.md 的时长维度）。

INSERT INTO billing_plans
  (plan_id, stripe_price_id, display_name, kind, included_quota_bytes,
   package_name, price_amount, price_currency, price_unit,
   price_multipliers, features, trial_days, active, sort_order)
VALUES
  -- free：体验档。高速流量按时间窗口给，不按字节。
  ('FREE', NULL, 'Free', 'subscription', 0,
   'free',
   0, '', '',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{
      "sla": "none",
      "session_persistence": false,
      "demo_cards": {"enabled": true, "create": true, "runs_per_day": 1, "session_minutes": 60},
      "fast_lane": {"mode": "windowed", "window": "weekly", "minutes": 60, "fallback": "vps"},
      "resource_cards": {"create": false},
      "dunning": {"policy": "none"}
    }'::jsonb,
   0, true, 10),

  -- Pay-As-You-Go：预充值按量扣费，欠费立即停机。
  -- 充值走 PAYG-TOPUP-* 那几条一次性价格，不在这条上挂 price。
  ('PAYG', NULL, 'Pay As You Go', 'subscription', 0,
   'payg',
   0, '', '',
   '{"fast_lane_cny_per_gb": 1.0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{
      "sla": "standard",
      "session_persistence": true,
      "resource_cards": {"create": true, "pricing": "list_price"},
      "retention": {"compute_days": 7, "object_storage_days": 30},
      "dunning": {"policy": "suspend_on_zero_balance", "grace_hours": 0}
    }'::jsonb,
   0, true, 20),

  -- PAYG 固定充值面额。Stripe Price ID 由目录同步后回填。
  ('PAYG-TOPUP-50', NULL, '余额充值 ¥50', 'paygo_topup', 0,
   'payg',
   5000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb": 1.0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"topup_amount": 50, "dunning": {"policy": "suspend_on_zero_balance", "grace_hours": 0}}'::jsonb,
   0, true, 21),

  ('PAYG-TOPUP-100', NULL, '余额充值 ¥100', 'paygo_topup', 0,
   'payg',
   10000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb": 1.0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"topup_amount": 100, "dunning": {"policy": "suspend_on_zero_balance", "grace_hours": 0}}'::jsonb,
   0, true, 22),

  ('PAYG-TOPUP-500', NULL, '余额充值 ¥500', 'paygo_topup', 0,
   'payg',
   50000, 'CNY', 'once',
   '{"fast_lane_cny_per_gb": 1.0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"topup_amount": 500, "dunning": {"policy": "suspend_on_zero_balance", "grace_hours": 0}}'::jsonb,
   0, true, 23),

  -- Pro 月付：¥20/月，含 20GB 高速流量，超出 1 元/GB。
  ('PRO-MONTHLY', NULL, 'Pro 月付', 'subscription', 21474836480,
   'pro',
   2000, 'CNY', 'month',
   '{"fast_lane_cny_per_gb": 1.0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{
      "sla": "standard",
      "session_persistence": true,
      "resource_cards": {"create": true, "pricing": "list_price_plus_managed_fee", "managed_fee_rate": 0.20},
      "fast_lane": {"mode": "quota"},
      "overage": {"policy": "charge", "cny_per_gb": 1.0},
      "dunning": {"policy": "grace_then_suspend", "grace_days": 14},
      "quota_cycle": "natural_month"
    }'::jsonb,
   0, true, 30),

  -- Pro 年付：¥200/年，配额仍按自然月发放 20GB（共 12 期，年累计 240GB）。
  -- quota_cycle=natural_month 是关键：Stripe 年付订阅的 current_period 是一年，
  -- 直接沿用会导致配额一年才重置一次。见 02 的"配额周期与 Stripe 周期解耦"。
  ('PRO-YEARLY', NULL, 'Pro 年付', 'subscription', 21474836480,
   'pro',
   20000, 'CNY', 'year',
   '{"fast_lane_cny_per_gb": 1.0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{
      "sla": "standard",
      "session_persistence": true,
      "resource_cards": {"create": true, "pricing": "list_price_plus_managed_fee", "managed_fee_rate": 0.20},
      "fast_lane": {"mode": "quota"},
      "overage": {"policy": "charge", "cny_per_gb": 1.0},
      "dunning": {"policy": "grace_then_suspend", "grace_days": 14},
      "quota_cycle": "natural_month"
    }'::jsonb,
   0, true, 40),

  -- 存量用户迁移专用过渡档（07-existing-user-migration.md）。
  -- 不上架（active=false）：它不是用户可选购的档位，只能由回填脚本/运营指派。
  -- overage.policy=none 是刻意的：观察期内不因超额产生任何实质影响，目标是先
  -- 摸清这批人的真实用量，而不是先收一轮钱。
  ('LEGACY-GRANDFATHERED', NULL, '存量用户过渡档', 'subscription', 0,
   'legacy',
   0, '', '',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{
      "sla": "standard",
      "session_persistence": true,
      "fast_lane": {"mode": "quota"},
      "overage": {"policy": "none"},
      "dunning": {"policy": "manual"},
      "grandfathered": {"reason": "pre-billing existing user"}
    }'::jsonb,
   0, false, 90)

ON CONFLICT (plan_id) DO UPDATE SET
  display_name         = EXCLUDED.display_name,
  kind                 = EXCLUDED.kind,
   included_quota_bytes = EXCLUDED.included_quota_bytes,
   package_name         = EXCLUDED.package_name,
   price_amount        = EXCLUDED.price_amount,
   price_currency      = EXCLUDED.price_currency,
   price_unit          = EXCLUDED.price_unit,
   price_multipliers    = EXCLUDED.price_multipliers,
  features             = EXCLUDED.features,
  trial_days           = EXCLUDED.trial_days,
  active               = EXCLUDED.active,
  sort_order           = EXCLUDED.sort_order,
  updated_at           = now();
-- 注意 ON CONFLICT 里**不更新** stripe_price_id：那个值由
-- scripts/stripe-sync-catalog.sh 的输出经 admin 接口回填，重跑本脚本不该把
-- 已经接好的 price id 抹回 NULL。

SELECT plan_id, kind, package_name, included_quota_bytes, active, sort_order,
       coalesce(stripe_price_id, '(unset)') AS stripe_price
FROM billing_plans
ORDER BY sort_order, plan_id;
