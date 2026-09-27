-- P0-Q1: local quota catalog for UAT.
--
-- This migration is deliberately additive and idempotent. It changes only the
-- catalog rows used to resolve future entitlements; it does not rewrite users,
-- subscriptions, payments, usage ledgers, or existing group membership.

BEGIN;

INSERT INTO public.billing_plans (
  plan_id,
  display_name,
  kind,
  included_quota_bytes,
  package_name,
  features,
  active,
  sort_order
)
VALUES
  (
    'FREE',
    'Free',
    'subscription',
    5368709120,
    'free',
    '{"quota_cycle":"natural_month","fast_lane":{"mode":"quota"},"dunning":{"policy":"downgrade_to_free"}}'::jsonb,
    TRUE,
    10
  ),
  (
    'PLUS',
    'Plus',
    'subscription',
    21474836480,
    'plus',
    '{"quota_cycle":"natural_month","fast_lane":{"mode":"quota"},"dunning":{"policy":"downgrade_to_free"}}'::jsonb,
    TRUE,
    20
  ),
  (
    'UNLIMITED-BETA',
    '无限制（内测）',
    'subscription',
    0,
    'unlimited-beta',
    '{"quota_cycle":"none","fast_lane":{"mode":"unlimited"},"internal_only":true,"dunning":{"policy":"manual"}}'::jsonb,
    TRUE,
    30
  )
ON CONFLICT (plan_id) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  kind = EXCLUDED.kind,
  included_quota_bytes = EXCLUDED.included_quota_bytes,
  package_name = EXCLUDED.package_name,
  features = EXCLUDED.features,
  active = EXCLUDED.active,
  sort_order = EXCLUDED.sort_order,
  updated_at = now();

COMMIT;
