CREATE TABLE IF NOT EXISTS public.traffic_stat_checkpoints (
  node_id TEXT NOT NULL,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  last_uplink_total BIGINT NOT NULL DEFAULT 0,
  last_downlink_total BIGINT NOT NULL DEFAULT 0,
  last_seen_at TIMESTAMPTZ NOT NULL,
  xray_revision TEXT NOT NULL DEFAULT '',
  reset_epoch BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (node_id, account_uuid)
);

CREATE TABLE IF NOT EXISTS public.traffic_minute_buckets (
  bucket_start TIMESTAMPTZ NOT NULL,
  node_id TEXT NOT NULL,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  region TEXT NOT NULL DEFAULT '',
  line_code TEXT NOT NULL DEFAULT '',
  uplink_bytes BIGINT NOT NULL DEFAULT 0,
  downlink_bytes BIGINT NOT NULL DEFAULT 0,
  total_bytes BIGINT NOT NULL DEFAULT 0,
  multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  rating_status TEXT NOT NULL DEFAULT 'pending',
  source_revision TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bucket_start, node_id, account_uuid, region, line_code)
);

CREATE TABLE IF NOT EXISTS public.billing_ledger (
  id UUID PRIMARY KEY,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  bucket_start TIMESTAMPTZ NOT NULL,
  bucket_end TIMESTAMPTZ NOT NULL,
  entry_type TEXT NOT NULL,
  rated_bytes BIGINT NOT NULL DEFAULT 0,
  amount_delta DOUBLE PRECISION NOT NULL DEFAULT 0,
  balance_after DOUBLE PRECISION NOT NULL DEFAULT 0,
  pricing_rule_version TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.account_quota_states (
  account_uuid UUID PRIMARY KEY REFERENCES public.users(uuid) ON DELETE CASCADE,
  remaining_included_quota BIGINT NOT NULL DEFAULT 0,
  current_balance DOUBLE PRECISION NOT NULL DEFAULT 0,
  arrears BOOLEAN NOT NULL DEFAULT false,
  arrears_since TIMESTAMPTZ NULL,
  throttle_state TEXT NOT NULL DEFAULT 'normal',
  suspend_state TEXT NOT NULL DEFAULT 'active',
  last_rated_bucket_at TIMESTAMPTZ NULL,
  effective_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE public.account_quota_states
  ADD COLUMN IF NOT EXISTS arrears_since TIMESTAMPTZ NULL;

CREATE TABLE IF NOT EXISTS public.account_billing_profiles (
  account_uuid UUID PRIMARY KEY REFERENCES public.users(uuid) ON DELETE CASCADE,
  package_name TEXT NOT NULL DEFAULT 'default',
  included_quota_bytes BIGINT NOT NULL DEFAULT 0,
  base_price_per_byte DOUBLE PRECISION NOT NULL DEFAULT 0,
  region_multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  line_multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  peak_multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  offpeak_multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  pricing_rule_version TEXT NOT NULL DEFAULT 'pricing-default-v1',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.billing_source_sync_state (
  source_id TEXT PRIMARY KEY,
  last_completed_until TIMESTAMPTZ NULL,
  last_attempted_at TIMESTAMPTZ NULL,
  last_succeeded_at TIMESTAMPTZ NULL,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.account_policy_snapshots (
  account_uuid UUID PRIMARY KEY REFERENCES public.users(uuid) ON DELETE CASCADE,
  policy_version TEXT NOT NULL,
  auth_state TEXT NOT NULL DEFAULT 'active',
  rate_profile TEXT NOT NULL DEFAULT 'standard',
  conn_profile TEXT NOT NULL DEFAULT 'standard',
  eligible_node_groups JSONB NOT NULL DEFAULT '[]'::jsonb,
  preferred_strategy TEXT NOT NULL DEFAULT 'ewma',
  degrade_mode TEXT NOT NULL DEFAULT 'deny',
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.node_health_snapshots (
  node_id TEXT PRIMARY KEY,
  region TEXT NOT NULL DEFAULT '',
  line_code TEXT NOT NULL DEFAULT '',
  pricing_group TEXT NOT NULL DEFAULT '',
  stats_enabled BOOLEAN NOT NULL DEFAULT false,
  xray_revision TEXT NOT NULL DEFAULT '',
  healthy BOOLEAN NOT NULL DEFAULT false,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  error_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
  active_connections INTEGER NOT NULL DEFAULT 0,
  health_score DOUBLE PRECISION NOT NULL DEFAULT 0,
  sampled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.scheduler_decisions (
  id UUID PRIMARY KEY,
  account_uuid UUID NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  node_group TEXT NOT NULL DEFAULT '',
  strategy TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL DEFAULT '',
  generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_traffic_minute_buckets_account_bucket
  ON public.traffic_minute_buckets (account_uuid, bucket_start DESC);

CREATE INDEX IF NOT EXISTS idx_billing_ledger_account_created
  ON public.billing_ledger (account_uuid, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_node_health_snapshots_sampled
  ON public.node_health_snapshots (sampled_at DESC);

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_generated
  ON public.scheduler_decisions (generated_at DESC);
