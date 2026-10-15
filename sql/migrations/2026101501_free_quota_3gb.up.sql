-- Free users receive 3 GiB per natural month. Migrate the previous quota
-- group and catalog value without changing paid plans or usage history.
BEGIN;
UPDATE public.users
SET groups = (
  SELECT jsonb_agg(CASE WHEN value = to_jsonb('segment:quota:free-5gb'::text) THEN to_jsonb('segment:quota:free-3gb'::text) ELSE value END)
  FROM jsonb_array_elements(groups) AS item(value)
)
WHERE groups ? 'segment:quota:free-5gb';
-- Preserve consumed bytes when reducing an already-granted Free allowance.
-- Catalog-managed paid and custom profiles are not changed.
UPDATE public.account_quota_states AS q
SET remaining_included_quota = GREATEST(3221225472 - GREATEST(
  p.included_quota_bytes - q.remaining_included_quota,
  COALESCE((SELECT SUM(b.total_bytes) FROM public.traffic_minute_buckets b
    WHERE b.account_uuid = q.account_uuid
      AND b.bucket_start >= COALESCE(q.period_start, date_trunc('month', now()))
      AND (q.period_end IS NULL OR b.bucket_start < q.period_end)), 0), 0), 0),
    updated_at = now()
FROM public.account_billing_profiles AS p
WHERE p.account_uuid = q.account_uuid AND p.pricing_rule_version = 'plan:FREE';
UPDATE public.account_billing_profiles
SET included_quota_bytes = 3221225472, updated_at = now()
WHERE pricing_rule_version = 'plan:FREE';
UPDATE public.billing_plans
SET included_quota_bytes = 3221225472, updated_at = now()
WHERE plan_id = 'FREE';
COMMIT;
