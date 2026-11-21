BEGIN;

ALTER TABLE public.traffic_minute_buckets
  DROP COLUMN IF EXISTS routing_pool;

ALTER TABLE public.account_quota_states
  DROP COLUMN IF EXISTS remaining_high_speed_quota;

ALTER TABLE public.account_billing_profiles
  DROP COLUMN IF EXISTS high_speed_quota_bytes;

ALTER TABLE public.billing_plans
  DROP COLUMN IF EXISTS high_speed_quota_bytes;

COMMIT;
