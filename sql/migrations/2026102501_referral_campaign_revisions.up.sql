-- Single configurable XConnect campaign. Never rewrite an earlier policy.
BEGIN;
CREATE TABLE IF NOT EXISTS public.referral_campaign_revisions (
 revision BIGSERIAL PRIMARY KEY,
 enabled BOOLEAN NOT NULL,
 starts_at TIMESTAMPTZ NULL,
 ends_at TIMESTAMPTZ NULL,
 effective_at TIMESTAMPTZ NOT NULL,
 actor_uuid UUID NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
 reason TEXT NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 cashback_bps INTEGER NOT NULL DEFAULT 1000 CHECK (cashback_bps=1000),
 annual_bonus_days INTEGER NOT NULL DEFAULT 30 CHECK (annual_bonus_days=30),
 CHECK (starts_at IS NULL OR ends_at IS NULL OR starts_at<ends_at)
);
CREATE INDEX IF NOT EXISTS referral_campaign_effective_idx ON public.referral_campaign_revisions(effective_at DESC,revision DESC);
INSERT INTO public.referral_campaign_revisions(enabled,starts_at,ends_at,effective_at,reason)
 SELECT true,'2026-10-13 00:00:00+08','2027-01-01 00:00:00+08','1970-01-01 00:00:00+00','initial XConnect campaign defaults'
 WHERE NOT EXISTS(SELECT 1 FROM public.referral_campaign_revisions);
CREATE TABLE IF NOT EXISTS public.referral_verified_invoices (
 invoice_id UUID PRIMARY KEY REFERENCES public.finance_invoices(id) ON DELETE RESTRICT,
 provider_price_id TEXT NOT NULL CHECK (length(provider_price_id)>0),
 plan_id TEXT NOT NULL CHECK (plan_id IN ('PRO-MONTHLY','PRO-YEARLY','PRO-MONTHLY-V2','PRO-YEARLY-V2')),
 product_slug TEXT NOT NULL DEFAULT 'xconnect' CHECK (product_slug='xconnect'),
 verified_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS public.referral_reward_campaign_snapshots (
 reward_id UUID PRIMARY KEY REFERENCES public.referral_rewards(id) ON DELETE RESTRICT,
 campaign_revision BIGINT NOT NULL REFERENCES public.referral_campaign_revisions(revision) ON DELETE RESTRICT
);
-- Existing rewards retain the original policy, irrespective of migration time.
INSERT INTO public.referral_reward_campaign_snapshots(reward_id,campaign_revision)
 SELECT r.id,c.revision FROM public.referral_rewards r
 CROSS JOIN LATERAL (SELECT revision FROM public.referral_campaign_revisions WHERE effective_at<=r.paid_at ORDER BY effective_at DESC,revision DESC LIMIT 1) c
 ON CONFLICT(reward_id) DO NOTHING;
CREATE OR REPLACE FUNCTION public.snapshot_referral_campaign() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE selected_revision BIGINT;
BEGIN
 SELECT revision INTO STRICT selected_revision FROM public.referral_campaign_revisions
 WHERE effective_at<=NEW.paid_at ORDER BY effective_at DESC,revision DESC LIMIT 1;
 INSERT INTO public.referral_reward_campaign_snapshots(reward_id,campaign_revision) VALUES(NEW.id,selected_revision);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS referral_reward_campaign_snapshot ON public.referral_rewards;
CREATE TRIGGER referral_reward_campaign_snapshot AFTER INSERT ON public.referral_rewards FOR EACH ROW EXECUTE FUNCTION public.snapshot_referral_campaign();
CREATE OR REPLACE FUNCTION public.reject_referral_campaign_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% is append-only; % is not permitted',TG_TABLE_NAME,TG_OP; END $$;
DROP TRIGGER IF EXISTS referral_campaign_revisions_append_only ON public.referral_campaign_revisions;
CREATE TRIGGER referral_campaign_revisions_append_only BEFORE UPDATE OR DELETE ON public.referral_campaign_revisions FOR EACH ROW EXECUTE FUNCTION public.reject_referral_campaign_mutation();
DROP TRIGGER IF EXISTS referral_campaign_snapshots_append_only ON public.referral_reward_campaign_snapshots;
CREATE TRIGGER referral_campaign_snapshots_append_only BEFORE UPDATE OR DELETE ON public.referral_reward_campaign_snapshots FOR EACH ROW EXECUTE FUNCTION public.reject_referral_campaign_mutation();
DROP TRIGGER IF EXISTS referral_verified_invoices_append_only ON public.referral_verified_invoices;
CREATE TRIGGER referral_verified_invoices_append_only BEFORE UPDATE OR DELETE ON public.referral_verified_invoices FOR EACH ROW EXECUTE FUNCTION public.reject_referral_campaign_mutation();
COMMIT;
