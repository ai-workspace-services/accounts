-- Free users receive 3 GiB per natural month. Migrate the previous quota
-- group and catalog value without changing paid plans or usage history.
BEGIN;
UPDATE public.users
SET groups = (
  SELECT jsonb_agg(CASE WHEN value = 'segment:quota:free-5gb' THEN to_jsonb('segment:quota:free-3gb'::text) ELSE value END)
  FROM jsonb_array_elements(groups) AS item(value)
)
WHERE groups ? 'segment:quota:free-5gb';
UPDATE public.billing_plans
SET included_quota_bytes = 3221225472, updated_at = now()
WHERE plan_id = 'FREE';
COMMIT;
