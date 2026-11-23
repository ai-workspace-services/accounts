-- 套餐目录种子数据（幂等，可反复执行）。
--
-- 用法：
--   psql -U postgres -d account -v ON_ERROR_STOP=1 -f scripts/seed-billing-plans.sql
--
-- 说明：
--   1. 前台商业系列仅包含 plus / pro / custom；whitelist_group (WHITELIST-FREE) 为 internal_only 运营白名单组。
--   2. stripe_price_id 具有 UNIQUE 约束，未绑定的价格保持 NULL（严禁写空串），已有值不覆盖。

INSERT INTO billing_plans (
  plan_id, stripe_price_id, display_name, kind, included_quota_bytes, high_speed_quota_bytes,
  package_name, price_amount, price_currency, price_unit, price_multipliers,
  features, trial_days, active, sort_order
)
VALUES
  ('WHITELIST-FREE', NULL, '白名单免费体验组', 'whitelist_group', 5368709120, 0,
   'whitelist', 0, 'CNY', 'month',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"family":"whitelist","tier_rank":0,"internal_only":true,"quota_cycle":"natural_month","allowed_pools":["Media-Pool"],"fast_lane":{"mode":"none"},"dunning":{"policy":"none","grace_days":3},"description":"白名单免费用户默认仅开放基础共享节点（Media-Pool），月度免费额度可在管理台调整。"}'::jsonb,
   0, true, 5),

  ('PLUS', NULL, 'Plus · 20 GB', 'subscription', 21474836480, 0,
   'plus', 2000, 'CNY', 'month',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"family":"plus","tier_rank":10,"audience":"订阅用户","quota_cycle":"natural_month","fast_lane":{"mode":"none"},"dunning":{"policy":"pause_traffic","grace_days":3},"comparison_matrix":{"resources":"Media-Pool 共享资源池","exit_ip":"共享出口"}}'::jsonb,
   0, true, 10),

  ('PLUS-MONTHLY-2026', NULL, 'Plus · 30 GB', 'subscription', 32212254720, 0,
   'plus', 3000, 'CNY', 'month',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"family":"plus","tier_rank":10,"audience":"订阅用户","quota_cycle":"natural_month","fast_lane":{"mode":"none"},"dunning":{"policy":"pause_traffic","grace_days":3},"comparison_matrix":{"resources":"Media-Pool 共享资源池","exit_ip":"共享出口"}}'::jsonb,
   0, true, 15),

  ('PRO-MONTHLY-V2', NULL, 'Pro 月付', 'subscription', 53687091200, 32212254720,
   'pro', 5000, 'CNY', 'month',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"family":"pro","tier_rank":20,"audience":"订阅用户","highlight":true,"quota_cycle":"natural_month","fast_lane":{"mode":"quota"},"dunning":{"policy":"pause_traffic","grace_days":3},"comparison_matrix":{"resources":"50GB Media-Pool + 30GB Pro-EC2-Pool","exit_ip":"Pro 专属高速节点"}}'::jsonb,
   0, true, 20),

  ('PRO-YEARLY-V2', NULL, 'Pro 年付', 'subscription', 53687091200, 32212254720,
   'pro', 50000, 'CNY', 'year',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"family":"pro","tier_rank":20,"audience":"订阅用户","quota_cycle":"natural_month","fast_lane":{"mode":"quota"},"dunning":{"policy":"pause_traffic","grace_days":3},"comparison_matrix":{"resources":"50GB/月 Media-Pool + 30GB/月 Pro-EC2-Pool","exit_ip":"Pro 专属高速节点"}}'::jsonb,
   0, true, 25),

  ('CUSTOM-NODE-MONTHLY-2026', NULL, '定制节点', 'subscription', 0, 0,
   'custom', 8000, 'CNY', 'month',
   '{"fast_lane_cny_per_gb": 0, "vps_lane_cny_per_gb": 0}'::jsonb,
   '{"family":"custom","tier_rank":30,"checkout_mode":"subscription","audience":"订阅用户","quota_label":"独享IP · 独享节点","dunning":{"policy":"pause_traffic","grace_days":3},"comparison_matrix":{"resources":"独享节点","exit_ip":"独享IP","ip_changes":"每个月免费更换一次，再次更换10元/次"}}'::jsonb,
   0, true, 40),

  ('CUSTOM-AI-CONSULT-2026', NULL, 'AI 工具咨询，电脑环境配置', 'service', 0, 0,
   'custom', 10000, 'CNY', 'once',
   '{}'::jsonb,
   '{"family":"custom","tier_rank":30,"checkout_mode":"payment","price_unit_label":"小时/次","audience":"面向非订阅用户的付费服务","description":"支持：MacOS Windows Linux IOS Android，不支持：国产华为纯血鸿蒙系统","dunning":{"policy":"none","grace_days":3},"comparison_matrix":{"support":"MacOS / Windows / Linux / IOS / Android（不支持国产华为纯血鸿蒙系统）"}}'::jsonb,
   0, true, 50)
ON CONFLICT (plan_id) DO UPDATE SET
  display_name           = EXCLUDED.display_name,
  kind                   = EXCLUDED.kind,
  included_quota_bytes   = EXCLUDED.included_quota_bytes,
  high_speed_quota_bytes = EXCLUDED.high_speed_quota_bytes,
  package_name           = EXCLUDED.package_name,
  price_amount           = EXCLUDED.price_amount,
  price_currency         = EXCLUDED.price_currency,
  price_unit             = EXCLUDED.price_unit,
  price_multipliers      = EXCLUDED.price_multipliers,
  features               = EXCLUDED.features,
  trial_days             = EXCLUDED.trial_days,
  active                 = EXCLUDED.active,
  sort_order             = EXCLUDED.sort_order,
  updated_at             = now();

SELECT plan_id, kind, package_name, included_quota_bytes, high_speed_quota_bytes, price_amount, price_unit, active, sort_order,
       coalesce(stripe_price_id, '(unset)') AS stripe_price
FROM billing_plans
ORDER BY sort_order, plan_id;
