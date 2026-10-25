-- Preserve policy and reward evidence after the feature is used.
BEGIN;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM public.referral_reward_campaign_snapshots)
 OR EXISTS(SELECT 1 FROM public.referral_verified_invoices)
 OR (SELECT count(*) FROM public.referral_campaign_revisions)>1 THEN
 RAISE EXCEPTION 'campaign rollback refused: reward snapshots or configuration history exist; use a reviewed forward migration';
 END IF;
END $$;
DROP TRIGGER IF EXISTS referral_reward_campaign_snapshot ON public.referral_rewards;
DROP FUNCTION IF EXISTS public.snapshot_referral_campaign();
DROP TABLE public.referral_reward_campaign_snapshots;
DROP TABLE public.referral_verified_invoices;
DROP TABLE public.referral_campaign_revisions;
DROP FUNCTION IF EXISTS public.reject_referral_campaign_mutation();
COMMIT;
