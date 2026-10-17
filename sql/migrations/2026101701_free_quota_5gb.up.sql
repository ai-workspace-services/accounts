-- Raise all classified Free accounts to 5 GiB, retaining current-month usage.
-- Keep the legacy group identifier stable for existing clients and admin APIs.
BEGIN;
SET LOCAL TIME ZONE 'UTC';
LOCK TABLE public.users, public.account_billing_profiles, public.account_quota_states
  IN SHARE ROW EXCLUSIVE MODE;

CREATE TEMP TABLE free_quota_upgrade ON COMMIT DROP AS
SELECT u.uuid,
  date_trunc('month', now()) AS period_start,
  date_trunc('month', now()) + interval '1 month' AS period_end,
  GREATEST(
    CASE WHEN q.period_end IS NULL OR q.period_end > date_trunc('month', now())
      THEN COALESCE(p.included_quota_bytes, 0) - COALESCE(q.remaining_included_quota, 0)
      ELSE 0 END,
    COALESCE((SELECT SUM(b.total_bytes) FROM public.traffic_minute_buckets b
      WHERE b.account_uuid=u.uuid AND b.bucket_start >= date_trunc('month', now())
        AND b.bucket_start < date_trunc('month', now()) + interval '1 month'), 0),
    0) AS used_bytes
FROM public.users u
LEFT JOIN public.account_billing_profiles p ON p.account_uuid=u.uuid
LEFT JOIN public.account_quota_states q ON q.account_uuid=u.uuid
WHERE u.groups ? 'segment:quota:free-3gb'
  AND lower(trim(u.role)) <> 'root'
  AND NOT (u.groups ?| ARRAY['segment:quota:plus-20gb', 'segment:quota:unlimited-beta',
                            'segment:subscription:unlimited'])
  AND (p.account_uuid IS NULL OR p.pricing_rule_version='plan:FREE')
  AND NOT EXISTS (SELECT 1 FROM public.network_team_members m
    WHERE m.user_uuid=u.uuid AND (m.dedicated_only OR m.team_id IS NOT NULL));

INSERT INTO public.account_billing_profiles
  (account_uuid, package_name, included_quota_bytes, pricing_rule_version)
SELECT uuid, 'free', 5368709120, 'plan:FREE' FROM free_quota_upgrade
ON CONFLICT (account_uuid) DO UPDATE
SET included_quota_bytes=EXCLUDED.included_quota_bytes, updated_at=now();

INSERT INTO public.account_quota_states
  (account_uuid, remaining_included_quota, period_start, period_end)
SELECT uuid, GREATEST(5368709120-used_bytes, 0), period_start, period_end
FROM free_quota_upgrade
ON CONFLICT (account_uuid) DO UPDATE
SET remaining_included_quota=EXCLUDED.remaining_included_quota,
    period_start=EXCLUDED.period_start, period_end=EXCLUDED.period_end,
    updated_at=now();

UPDATE public.billing_plans
SET included_quota_bytes=5368709120,
    features=jsonb_set(COALESCE(features, '{}'::jsonb), '{quota_cycle}', '"natural_month"'),
    updated_at=now()
WHERE plan_id='FREE';
COMMIT;
