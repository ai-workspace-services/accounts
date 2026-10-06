-- Billing-owned additive native schema. Accounts owns the shared accounting tables.
CREATE TABLE public.cloud_vendor_costs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  provider VARCHAR(50) NOT NULL,            -- aws, gcp, azure, vultr, etc.
  account_id VARCHAR(100) NOT NULL,         -- AWS Account ID, GCP Project ID, Azure Subscription ID
  service_name VARCHAR(100) NOT NULL,       -- e.g., AmazonEC2, Compute Engine
  region VARCHAR(100) NOT NULL DEFAULT '',  -- e.g., us-east-1
  usage_start_time TIMESTAMPTZ NOT NULL,
  usage_end_time TIMESTAMPTZ NOT NULL,
  cost_amount DOUBLE PRECISION NOT NULL,
  currency VARCHAR(10) NOT NULL DEFAULT 'USD',
  usage_quantity DOUBLE PRECISION,
  usage_unit VARCHAR(50),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Upsert uniqueness constraint for idempotency
  CONSTRAINT uq_cloud_vendor_cost_period UNIQUE (provider, account_id, service_name, region, usage_start_time)
);

CREATE INDEX idx_cloud_vendor_costs_time ON public.cloud_vendor_costs (usage_start_time DESC);
CREATE INDEX idx_cloud_vendor_costs_provider ON public.cloud_vendor_costs (provider);
