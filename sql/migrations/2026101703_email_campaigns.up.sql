-- Durable announcement queue and opt-in onboarding; no existing users are enrolled.
CREATE TABLE IF NOT EXISTS public.email_campaigns (
 id TEXT PRIMARY KEY,
 version BIGINT NOT NULL,
 kind TEXT NOT NULL,
 subject TEXT NOT NULL,
 body TEXT NOT NULL,
 action_label TEXT NOT NULL,
 action_url TEXT NOT NULL,
 audience TEXT NOT NULL,
 delivery_mode TEXT NOT NULL,
 status TEXT NOT NULL,
 created_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS public.email_onboarding_steps (
 id TEXT PRIMARY KEY,
 version BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL,
 subject TEXT NOT NULL,
 body TEXT NOT NULL,
 action_label TEXT NOT NULL,
 action_url TEXT NOT NULL,
 delay_hours BIGINT NOT NULL,
 activated_at TIMESTAMPTZ NOT NULL,
 updated_by TEXT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS public.email_deliveries (
 id TEXT PRIMARY KEY,
 dedupe_key TEXT NOT NULL,
 campaign_id TEXT NOT NULL,
 step_id TEXT NOT NULL,
 user_id TEXT NOT NULL,
 email TEXT NOT NULL,
 subject TEXT NOT NULL,
 body TEXT NOT NULL,
 action_label TEXT NOT NULL,
 action_url TEXT NOT NULL,
 category TEXT NOT NULL,
 delivery_mode TEXT NOT NULL,
 audience TEXT NOT NULL,
 status TEXT NOT NULL,
 attempts BIGINT NOT NULL,
 due_at TIMESTAMPTZ NOT NULL,
 lease_token TEXT NOT NULL,
 lease_until TIMESTAMPTZ,
 sent_at TIMESTAMPTZ,
 last_error TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS public.email_recipient_leases (
 user_id TEXT PRIMARY KEY,
 token TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS public.email_suppressions (
 email TEXT PRIMARY KEY,
 reason TEXT NOT NULL,
 updated_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_email_campaigns_status ON public.email_campaigns(status);
CREATE INDEX IF NOT EXISTS idx_email_deliveries_campaign_id ON public.email_deliveries(campaign_id);
CREATE INDEX IF NOT EXISTS idx_email_deliveries_user_id ON public.email_deliveries(user_id);
CREATE INDEX IF NOT EXISTS idx_email_deliveries_status ON public.email_deliveries(status);
CREATE INDEX IF NOT EXISTS idx_email_deliveries_due_at ON public.email_deliveries(due_at);
CREATE INDEX IF NOT EXISTS idx_email_deliveries_sent_at ON public.email_deliveries(sent_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_email_deliveries_dedupe_key ON public.email_deliveries(dedupe_key);
CREATE UNIQUE INDEX IF NOT EXISTS idx_email_delivery_identity ON public.email_deliveries(campaign_id, step_id, user_id);
