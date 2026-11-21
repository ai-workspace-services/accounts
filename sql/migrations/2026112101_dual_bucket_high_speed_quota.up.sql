BEGIN;

ALTER TABLE public.billing_plans
  ADD COLUMN IF NOT EXISTS high_speed_quota_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE public.account_billing_profiles
  ADD COLUMN IF NOT EXISTS high_speed_quota_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE public.account_quota_states
  ADD COLUMN IF NOT EXISTS remaining_high_speed_quota BIGINT NOT NULL DEFAULT 0;

ALTER TABLE public.traffic_minute_buckets
  ADD COLUMN IF NOT EXISTS routing_pool TEXT NOT NULL DEFAULT '';

-- 50 元 Pro 月付：30 GiB 高速流量 (Pro-EC2-Pool) + 50 GiB VPS 流量 (Media-Pool)
UPDATE public.billing_plans
SET high_speed_quota_bytes = 32212254720,
    updated_at = now()
WHERE plan_id IN ('PRO-MONTHLY-V2', 'PRO-MONTHLY-2026')
  AND high_speed_quota_bytes = 0;

UPDATE public.billing_plans
SET high_speed_quota_bytes = 9223372036854775807,
    updated_at = now()
WHERE plan_id = 'PRO-YEARLY-V2'
  AND high_speed_quota_bytes = 0;

-- 幂等回填存量活跃 Pro 用户当前周期的高速流量配置（不动 VPS 配额余额）
UPDATE public.account_billing_profiles p
SET high_speed_quota_bytes = CASE
      WHEN upper(p.pricing_rule_version) = 'PLAN:PRO-YEARLY-V2' THEN 9223372036854775807
      ELSE 32212254720
    END,
    updated_at = now()
WHERE lower(p.package_name) = 'pro'
  AND p.high_speed_quota_bytes = 0
  AND EXISTS (
    SELECT 1
    FROM public.users u
    WHERE u.uuid = p.account_uuid
      AND u.active
  );

UPDATE public.account_quota_states q
SET remaining_high_speed_quota = p.high_speed_quota_bytes,
    updated_at = now()
FROM public.account_billing_profiles p
WHERE q.account_uuid = p.account_uuid
  AND p.high_speed_quota_bytes > 0
  AND q.remaining_high_speed_quota = 0
  AND EXISTS (
    SELECT 1
    FROM public.users u
    WHERE u.uuid = q.account_uuid
      AND u.active
  );

COMMIT;
