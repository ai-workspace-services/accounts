-- 2026112401_cleanup_legacy_free_profiles_and_segments.up.sql
--
-- 清理历史遗留的 plan:FREE（3 GB 已耗尽暂停态）与废弃的免费分段标签：
-- 1. 从 public.users.groups 移除废弃标签 segment:free / segment:quota:free-3gb / segment:quota:free-5gb
--    （保留 Admin / User / segment:subscribed / segment:whitelist:free / segment:trial:once-5gb 等有效标签）。
-- 2. 对仍残留 package_name IN ('free','payg') 或 pricing_rule_version IN ('plan:FREE','plan:PAYG')
--    且无有效付费订阅 / 无团队成员 / 无自定义与白名单豁免的账户（含 root/admin 运维账号），
--    统一切换为暂停流量态（package_name='paused', included_quota_bytes=0, pricing_rule_version='plan:PAUSED',
--    proxy_access_state='paused', period_start=NULL, period_end=NULL）。

BEGIN;

-- 1. 清理 public.users.groups 中遗留的免费分段标签
UPDATE public.users u
SET groups = COALESCE((
      SELECT jsonb_agg(elem)
      FROM jsonb_array_elements_text(COALESCE(u.groups::jsonb, '[]'::jsonb)) elem
      WHERE elem NOT IN ('segment:free', 'segment:quota:free-3gb', 'segment:quota:free-5gb')
    ), '[]'::jsonb),
    updated_at = now()
WHERE COALESCE(u.groups::jsonb, '[]'::jsonb) ?| array['segment:free', 'segment:quota:free-3gb', 'segment:quota:free-5gb'];

-- 2. 将无有效付费/定制/白名单权益的遗留 plan:FREE / plan:PAYG 账户切换至 plan:PAUSED（含 root/admin）
DO $$
DECLARE
  rec record;
BEGIN
  FOR rec IN
    SELECT u.uuid
    FROM public.users u
    JOIN public.account_billing_profiles p ON p.account_uuid = u.uuid
    WHERE (
      lower(COALESCE(p.package_name, '')) IN ('free', 'payg')
      OR upper(COALESCE(p.pricing_rule_version, '')) IN ('PLAN:FREE', 'PLAN:PAYG')
    )
    AND NOT EXISTS (
      SELECT 1 FROM public.subscriptions s
      WHERE s.user_uuid = u.uuid
        AND lower(s.status) IN ('active', 'trialing', 'past_due', 'canceling')
        AND upper(COALESCE(s.plan_id, '')) NOT IN ('FREE', 'PAYG', 'TRIAL-5GB-ONCE', 'WHITELIST-FREE', 'PAUSED')
    )
    AND NOT EXISTS (
      SELECT 1 FROM public.network_team_members m
      WHERE m.user_uuid = u.uuid
    )
    AND NOT (COALESCE(u.groups::jsonb, '[]'::jsonb) ?| array[
      'segment:subscription:unlimited_override',
      'segment:custom',
      'segment:whitelist:free',
      'segment:trial:once-5gb',
      'subscription_source_plan:CUSTOM'
    ])
  LOOP
    UPDATE public.account_billing_profiles
    SET package_name           = 'paused',
        included_quota_bytes   = 0,
        high_speed_quota_bytes = 0,
        pricing_rule_version   = 'plan:PAUSED',
        updated_at             = now()
    WHERE account_uuid = rec.uuid;

    INSERT INTO public.account_quota_states (
      account_uuid, remaining_included_quota, remaining_high_speed_quota,
      arrears, arrears_since, throttle_state, suspend_state, proxy_access_state,
      period_start, period_end, effective_at
    )
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
