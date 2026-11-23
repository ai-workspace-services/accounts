BEGIN;

-- 1. 清理 18 个历史/重复/PAYG/Free 套餐行（保留 PLUS, PLUS-MONTHLY-2026, PRO-MONTHLY-V2, PRO-YEARLY-V2, CUSTOM-NODE-MONTHLY-2026, CUSTOM-AI-CONSULT-2026）
DELETE FROM public.billing_plans
WHERE plan_id IN (
  'FREE', 'FREE-3GB-2026', 'FREE-1GB-2026',
  'PAYG', 'PAYG-2026',
  'PAYG-TOPUP-50', 'PAYG-TOPUP-100', 'PAYG-TOPUP-200', 'PAYG-TOPUP-500',
  'PAYG-TOPUP-USD-20', 'PAYG-TOPUP-USD-50', 'PAYG-TOPUP-USD-100',
  'UNLIMITED-BETA', 'TRAFFIC-GIFT-2026', 'LEGACY-GRANDFATHERED',
  'PRO-MONTHLY', 'PRO-YEARLY', 'PRO-MONTHLY-2026'
);

-- 2. 幂等初始化/校准保留目录 + WHITELIST-FREE（不覆盖已绑定 stripe_price_id）
INSERT INTO public.billing_plans (
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

-- 修复因 2026112101 脚本按 9223372036854775807 回填的活跃 Pro 账号配额（若存在）
UPDATE public.account_billing_profiles
SET high_speed_quota_bytes = 32212254720,
    updated_at = now()
WHERE lower(package_name) = 'pro'
  AND high_speed_quota_bytes = 9223372036854775807;

UPDATE public.account_quota_states
SET remaining_high_speed_quota = 32212254720,
    updated_at = now()
WHERE remaining_high_speed_quota > 32212254720
  AND account_uuid IN (
    SELECT account_uuid FROM public.account_billing_profiles WHERE lower(package_name) = 'pro'
  );

-- 3. 彻底删除 sandbox@svc.plus（先清理 RESTRICT/NO ACTION 外键子行，再删用户并级联其余表）
DO $$
DECLARE
  v_sandbox_uuid uuid;
BEGIN
  SELECT uuid INTO v_sandbox_uuid
  FROM public.users
  WHERE lower(email) = 'sandbox@svc.plus'
  LIMIT 1;

  IF v_sandbox_uuid IS NOT NULL THEN
    DELETE FROM public.referral_verified_invoices
    WHERE invoice_id IN (SELECT id FROM public.finance_invoices WHERE account_uuid = v_sandbox_uuid);

    DELETE FROM public.finance_refunds
    WHERE payment_id IN (SELECT id FROM public.finance_payments WHERE account_uuid = v_sandbox_uuid);

    DELETE FROM public.finance_payments WHERE account_uuid = v_sandbox_uuid;
    DELETE FROM public.finance_invoices WHERE account_uuid = v_sandbox_uuid;

    DELETE FROM public.referral_settlement_items
    WHERE reward_id IN (SELECT id FROM public.referral_rewards WHERE referrer_uuid = v_sandbox_uuid OR invitee_uuid = v_sandbox_uuid)
       OR settlement_id IN (SELECT id FROM public.referral_settlements WHERE referrer_uuid = v_sandbox_uuid OR completed_by = v_sandbox_uuid);

    DELETE FROM public.subscription_extension_ledger
    WHERE user_uuid = v_sandbox_uuid
       OR reward_id IN (SELECT id FROM public.referral_rewards WHERE referrer_uuid = v_sandbox_uuid OR invitee_uuid = v_sandbox_uuid)
       OR subscription_uuid IN (SELECT uuid FROM public.subscriptions WHERE user_uuid = v_sandbox_uuid);

    DELETE FROM public.referral_reward_campaign_snapshots
    WHERE reward_id IN (SELECT id FROM public.referral_rewards WHERE referrer_uuid = v_sandbox_uuid OR invitee_uuid = v_sandbox_uuid)
       OR campaign_revision IN (SELECT revision FROM public.referral_campaign_revisions WHERE actor_uuid = v_sandbox_uuid);

    DELETE FROM public.referral_reward_events
    WHERE actor_uuid = v_sandbox_uuid
       OR reward_id IN (SELECT id FROM public.referral_rewards WHERE referrer_uuid = v_sandbox_uuid OR invitee_uuid = v_sandbox_uuid);

    DELETE FROM public.referral_rewards WHERE referrer_uuid = v_sandbox_uuid OR invitee_uuid = v_sandbox_uuid;
    DELETE FROM public.referral_settlements WHERE referrer_uuid = v_sandbox_uuid OR completed_by = v_sandbox_uuid;
    DELETE FROM public.referrals WHERE referrer_uuid = v_sandbox_uuid OR invitee_uuid = v_sandbox_uuid;
    DELETE FROM public.referral_codes WHERE user_uuid = v_sandbox_uuid;
    DELETE FROM public.referral_campaign_revisions WHERE actor_uuid = v_sandbox_uuid;
    DELETE FROM public.network_team_members WHERE user_uuid = v_sandbox_uuid;
    DELETE FROM public.account_lifecycle_events WHERE user_uuid = v_sandbox_uuid;
    DELETE FROM public.mfa_recovery_codes WHERE user_uuid = v_sandbox_uuid;
    DELETE FROM public.mfa_reset_requests WHERE user_uuid = v_sandbox_uuid OR reviewer_uuid = v_sandbox_uuid;
    DELETE FROM public.password_recovery_challenges WHERE user_uuid = v_sandbox_uuid;
    DELETE FROM public.subscription_lifecycle_states WHERE account_uuid = v_sandbox_uuid;

    DELETE FROM public.users WHERE uuid = v_sandbox_uuid;
  END IF;
END $$;

-- 4. 测试账号 haitaopan@xworktech.com：清空所有套餐、订阅与钱包余额，转入暂停流量模式
DO $$
DECLARE
  v_haitao_uuid uuid;
  v_groups jsonb;
BEGIN
  SELECT uuid, groups INTO v_haitao_uuid, v_groups
  FROM public.users
  WHERE lower(email) = 'haitaopan@xworktech.com'
  LIMIT 1;

  IF v_haitao_uuid IS NOT NULL THEN
    DELETE FROM public.subscriptions WHERE user_uuid = v_haitao_uuid;
    DELETE FROM public.subscription_records WHERE user_uuid = v_haitao_uuid;

    UPDATE public.users
    SET groups = COALESCE((
          SELECT jsonb_agg(elem)
          FROM jsonb_array_elements_text(COALESCE(v_groups, '[]'::jsonb)) elem
          WHERE elem NOT IN ('segment:free', 'segment:subscribed', 'segment:monthly', 'segment:yearly', 'segment:quota:free-3gb', 'segment:quota:free-5gb', 'segment:quota:plus-20gb', 'segment:quota:unlimited-beta', 'segment:subscription:monthly', 'segment:subscription:yearly', 'segment:subscription:listed', 'segment:subscription:unlimited_override', 'segment:whitelist:free', 'segment:trial:once-5gb')
            AND elem NOT LIKE 'subscription_source_plan:%'
        ), '[]'::jsonb),
        subscription_valid_from = NULL,
        subscription_valid_until = NULL,
        updated_at = now()
    WHERE uuid = v_haitao_uuid;

    INSERT INTO public.account_billing_profiles (account_uuid, package_name, included_quota_bytes, high_speed_quota_bytes, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version)
    VALUES (v_haitao_uuid, 'paused', 0, 0, 1.0, 1.0, 1.0, 1.0, 'plan:PAUSED')
    ON CONFLICT (account_uuid) DO UPDATE SET
      package_name           = 'paused',
      included_quota_bytes   = 0,
      high_speed_quota_bytes = 0,
      pricing_rule_version   = 'plan:PAUSED',
      updated_at             = now();

    INSERT INTO public.account_quota_states (account_uuid, current_balance, remaining_included_quota, remaining_high_speed_quota, arrears, arrears_since, throttle_state, suspend_state, proxy_access_state, period_start, period_end, effective_at)
    VALUES (v_haitao_uuid, 0, 0, 0, false, NULL, 'normal', 'active', 'paused', NULL, NULL, now())
    ON CONFLICT (account_uuid) DO UPDATE SET
      current_balance            = 0,
      remaining_included_quota   = 0,
      remaining_high_speed_quota = 0,
      arrears                    = false,
      arrears_since              = NULL,
      throttle_state             = 'normal',
      suspend_state              = CASE WHEN public.account_quota_states.suspend_state = 'suspended' THEN 'suspended' ELSE 'active' END,
      proxy_access_state         = 'paused',
      period_start               = NULL,
      period_end                 = NULL,
      effective_at               = now(),
      updated_at                 = now();
  END IF;
END $$;

-- 5. 首批白名单免费体验组：1195129840@qq.com、624220860@qq.com、review@svc.plus（配额读取 WHITELIST-FREE 配置表）
DO $$
DECLARE
  v_wl_quota bigint;
  v_period_start timestamptz := (date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC');
  v_period_end   timestamptz := ((date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC');
  rec record;
  v_rem bigint;
BEGIN
  SELECT COALESCE(included_quota_bytes, 5368709120) INTO v_wl_quota
  FROM public.billing_plans
  WHERE plan_id = 'WHITELIST-FREE'
  LIMIT 1;
  IF v_wl_quota IS NULL THEN
    v_wl_quota := 5368709120;
  END IF;

  FOR rec IN
    SELECT u.uuid, u.groups, p.package_name, p.included_quota_bytes, p.pricing_rule_version,
           q.remaining_included_quota, q.period_start AS q_period_start
    FROM public.users u
    LEFT JOIN public.account_billing_profiles p ON p.account_uuid = u.uuid
    LEFT JOIN public.account_quota_states q ON q.account_uuid = u.uuid
    WHERE lower(u.email) IN ('1195129840@qq.com', '624220860@qq.com', 'review@svc.plus')
  LOOP
    UPDATE public.users
    SET groups = (
          SELECT jsonb_agg(DISTINCT g)
          FROM (
            SELECT elem AS g
            FROM jsonb_array_elements_text(COALESCE(rec.groups, '[]'::jsonb)) elem
            WHERE elem NOT IN ('segment:free', 'segment:quota:free-3gb', 'segment:quota:free-5gb', 'segment:trial:once-5gb')
            UNION ALL
            SELECT 'segment:whitelist:free'
          ) t
        ),
        updated_at = now()
    WHERE uuid = rec.uuid;

    IF rec.pricing_rule_version = 'plan:WHITELIST-FREE' AND rec.q_period_start = v_period_start AND rec.remaining_included_quota IS NOT NULL THEN
      v_rem := LEAST(v_wl_quota, GREATEST(0, rec.remaining_included_quota));
    ELSIF lower(COALESCE(rec.package_name, '')) IN ('free', 'whitelist') AND COALESCE(rec.included_quota_bytes, 0) > 0 AND rec.remaining_included_quota IS NOT NULL THEN
      v_rem := GREATEST(0, v_wl_quota - GREATEST(0, rec.included_quota_bytes - rec.remaining_included_quota));
    ELSE
      v_rem := v_wl_quota;
    END IF;

    INSERT INTO public.account_billing_profiles (account_uuid, package_name, included_quota_bytes, high_speed_quota_bytes, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version)
    VALUES (rec.uuid, 'whitelist', v_wl_quota, 0, 1.0, 1.0, 1.0, 1.0, 'plan:WHITELIST-FREE')
    ON CONFLICT (account_uuid) DO UPDATE SET
      package_name           = 'whitelist',
      included_quota_bytes   = v_wl_quota,
      high_speed_quota_bytes = 0,
      pricing_rule_version   = 'plan:WHITELIST-FREE',
      updated_at             = now();

    INSERT INTO public.account_quota_states (account_uuid, remaining_included_quota, remaining_high_speed_quota, arrears, arrears_since, throttle_state, suspend_state, proxy_access_state, period_start, period_end, effective_at)
    VALUES (rec.uuid, v_rem, 0, false, NULL, 'normal', 'active', CASE WHEN v_rem > 0 THEN 'active' ELSE 'paused' END, v_period_start, v_period_end, now())
    ON CONFLICT (account_uuid) DO UPDATE SET
      remaining_included_quota   = v_rem,
      remaining_high_speed_quota = 0,
      arrears                    = false,
      arrears_since              = NULL,
      throttle_state             = 'normal',
      suspend_state              = CASE WHEN public.account_quota_states.suspend_state = 'suspended' THEN 'suspended' ELSE 'active' END,
      proxy_access_state         = CASE WHEN v_rem > 0 THEN 'active' ELSE 'paused' END,
      period_start               = v_period_start,
      period_end                 = v_period_end,
      effective_at               = now(),
      updated_at                 = now();
  END LOOP;
END $$;

-- 6. 美区定制 Custom 1024 GiB 已付费用户（3667108906@qq.com、ho_yatphone@163.com）：修正遗留 plan:FREE 标签，永不暂停
UPDATE public.users
SET groups = (
      SELECT jsonb_agg(DISTINCT g)
      FROM (
        SELECT elem AS g
        FROM jsonb_array_elements_text(COALESCE(groups, '[]'::jsonb)) elem
        WHERE elem NOT IN ('segment:free', 'segment:quota:free-3gb', 'segment:quota:free-5gb', 'segment:trial:once-5gb')
        UNION ALL
        SELECT 'segment:subscribed'
        UNION ALL
        SELECT 'subscription_source_plan:CUSTOM'
      ) t
    ),
    updated_at = now()
WHERE lower(email) IN ('3667108906@qq.com', 'ho_yatphone@163.com');

UPDATE public.account_billing_profiles p
SET package_name = 'custom',
    high_speed_quota_bytes = 0,
    pricing_rule_version = 'plan:CUSTOM',
    updated_at = now()
FROM public.users u
WHERE p.account_uuid = u.uuid
  AND lower(u.email) IN ('3667108906@qq.com', 'ho_yatphone@163.com');

UPDATE public.account_quota_states q
SET arrears = false,
    arrears_since = NULL,
    throttle_state = 'normal',
    suspend_state = 'active',
    proxy_access_state = 'active',
    updated_at = now()
FROM public.users u
WHERE q.account_uuid = u.uuid
  AND lower(u.email) IN ('3667108906@qq.com', 'ho_yatphone@163.com');

-- 7. 存量 11 位老 Plus 用户（20 GiB -> 30 GiB/月，不额外收费；不动 Stripe subscriptions 行）
UPDATE public.account_quota_states q
SET remaining_included_quota = LEAST(32212254720, GREATEST(0, q.remaining_included_quota) + 10737418240),
    proxy_access_state = CASE
      WHEN q.remaining_included_quota <= 0 AND q.proxy_access_state = 'paused' AND q.suspend_state <> 'suspended' THEN 'active'
      ELSE q.proxy_access_state
    END,
    updated_at = now()
FROM public.account_billing_profiles p
JOIN public.users u ON u.uuid = p.account_uuid
WHERE q.account_uuid = p.account_uuid
  AND lower(p.package_name) = 'plus'
  AND p.included_quota_bytes = 21474836480
  AND lower(u.role) NOT IN ('root', 'admin')
  AND COALESCE(u.level, 0) < 100
  AND lower(u.email) NOT IN ('3667108906@qq.com', 'ho_yatphone@163.com', 'haitaopan@xworktech.com', 'sandbox@svc.plus', 'review@svc.plus');

UPDATE public.account_billing_profiles p
SET included_quota_bytes = 32212254720,
    pricing_rule_version = 'plan:PLUS-MONTHLY-2026',
    updated_at = now()
FROM public.users u
WHERE p.account_uuid = u.uuid
  AND lower(p.package_name) = 'plus'
  AND p.included_quota_bytes = 21474836480
  AND lower(u.role) NOT IN ('root', 'admin')
  AND COALESCE(u.level, 0) < 100
  AND lower(u.email) NOT IN ('3667108906@qq.com', 'ho_yatphone@163.com', 'haitaopan@xworktech.com', 'sandbox@svc.plus', 'review@svc.plus');

UPDATE public.subscription_records sr
SET monthly_quota_bytes = 32212254720,
    note = CASE
      WHEN COALESCE(sr.note, '') LIKE '%老用户Plus免费升级30GB%' THEN sr.note
      WHEN COALESCE(trim(sr.note), '') = '' THEN '老用户Plus免费升级30GB（使用老用户优惠追平账单）'
      ELSE sr.note || ' | 老用户Plus免费升级30GB（使用老用户优惠追平账单）'
    END,
    updated_at = now()
FROM public.account_billing_profiles p
JOIN public.users u ON u.uuid = p.account_uuid
WHERE sr.user_uuid = u.uuid
  AND lower(p.package_name) = 'plus'
  AND p.included_quota_bytes = 32212254720
  AND sr.monthly_quota_bytes = 21474836480
  AND lower(u.role) NOT IN ('root', 'admin')
  AND COALESCE(u.level, 0) < 100
  AND lower(u.email) NOT IN ('3667108906@qq.com', 'ho_yatphone@163.com', 'haitaopan@xworktech.com', 'sandbox@svc.plus', 'review@svc.plus');

-- 8. 其余存量非白名单 Free 用户（不发放 5GB 一次性体验，全部直接转入「暂停流量」模式）
DO $$
DECLARE
  rec record;
BEGIN
  FOR rec IN
    SELECT u.uuid, u.groups
    FROM public.users u
    LEFT JOIN public.account_billing_profiles p ON p.account_uuid = u.uuid
    WHERE lower(u.role) NOT IN ('root', 'admin')
      AND COALESCE(u.level, 0) < 100
      AND lower(u.email) NOT IN (
        '1195129840@qq.com', '624220860@qq.com', 'review@svc.plus',
        '3667108906@qq.com', 'ho_yatphone@163.com',
        'haitaopan@xworktech.com', 'sandbox@svc.plus'
      )
      AND lower(COALESCE(p.package_name, 'free')) NOT IN ('plus', 'pro', 'custom', 'unlimited', 'whitelist')
      AND NOT EXISTS (
        SELECT 1 FROM public.subscriptions s
        WHERE s.user_uuid = u.uuid
          AND lower(s.status) IN ('active', 'trialing', 'past_due', 'canceling')
      )
      AND NOT EXISTS (
        SELECT 1 FROM public.network_team_members m
        WHERE m.user_uuid = u.uuid
      )
      AND NOT (COALESCE(u.groups::jsonb, '[]'::jsonb) ?| array['segment:subscription:unlimited_override', 'segment:custom', 'segment:whitelist:free'])
  LOOP
    UPDATE public.users
    SET groups = COALESCE((
          SELECT jsonb_agg(elem)
          FROM jsonb_array_elements_text(COALESCE(rec.groups, '[]'::jsonb)) elem
          WHERE elem NOT IN ('segment:free', 'segment:quota:free-3gb', 'segment:quota:free-5gb', 'segment:trial:once-5gb')
        ), '[]'::jsonb),
        updated_at = now()
    WHERE uuid = rec.uuid;

    INSERT INTO public.account_billing_profiles (account_uuid, package_name, included_quota_bytes, high_speed_quota_bytes, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version)
    VALUES (rec.uuid, 'paused', 0, 0, 1.0, 1.0, 1.0, 1.0, 'plan:PAUSED')
    ON CONFLICT (account_uuid) DO UPDATE SET
      package_name           = 'paused',
      included_quota_bytes   = 0,
      high_speed_quota_bytes = 0,
      pricing_rule_version   = 'plan:PAUSED',
      updated_at             = now();

    INSERT INTO public.account_quota_states (account_uuid, remaining_included_quota, remaining_high_speed_quota, arrears, arrears_since, throttle_state, suspend_state, proxy_access_state, period_start, period_end, effective_at)
    VALUES (rec.uuid, 0, 0, false, NULL, 'normal', 'active', 'paused', NULL, NULL, now())
    ON CONFLICT (account_uuid) DO UPDATE SET
      remaining_included_quota   = 0,
      remaining_high_speed_quota = 0,
      arrears                    = false,
      arrears_since              = NULL,
      throttle_state             = 'normal',
      suspend_state              = CASE WHEN public.account_quota_states.suspend_state = 'suspended' THEN 'suspended' ELSE 'active' END,
      proxy_access_state         = 'paused',
      period_start               = NULL,
      period_end                 = NULL,
      effective_at               = now(),
      updated_at                 = now();
  END LOOP;
END $$;

COMMIT;
