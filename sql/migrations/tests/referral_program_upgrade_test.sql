\set ON_ERROR_STOP on
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE public.users (uuid UUID PRIMARY KEY DEFAULT gen_random_uuid());
CREATE TABLE public.subscriptions (uuid UUID PRIMARY KEY DEFAULT gen_random_uuid());
CREATE TABLE public.finance_invoices (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), idempotency_key TEXT NOT NULL UNIQUE, account_uuid UUID NOT NULL, subscription_uuid UUID NOT NULL, provider TEXT NOT NULL, provider_invoice_id TEXT NOT NULL UNIQUE, amount_minor BIGINT NOT NULL, currency TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', issued_at TIMESTAMPTZ NOT NULL DEFAULT now(), due_at TIMESTAMPTZ NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
\ir ../2026101301_referral_program.up.sql
INSERT INTO public.users(uuid) VALUES
 ('00000000-0000-4000-8000-000000000001'),
 ('00000000-0000-4000-8000-000000000002');
INSERT INTO public.subscriptions(uuid) VALUES ('00000000-0000-4000-8000-000000000003');
INSERT INTO public.referral_codes(user_uuid,code) VALUES ('00000000-0000-4000-8000-000000000001','REFCODE1');
INSERT INTO public.referrals(referrer_uuid,invitee_uuid,referral_code)
 VALUES ('00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002','REFCODE1');
DO $$ BEGIN
  BEGIN
    INSERT INTO public.referrals(referrer_uuid,invitee_uuid,referral_code)
    VALUES ('00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000001','REFCODE1');
    RAISE EXCEPTION 'self referral unexpectedly accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
END $$;
INSERT INTO public.referral_rewards(referral_id,provider_invoice_id,referrer_uuid,invitee_uuid,plan_id,product_slug,paid_minor,reward_minor,currency,paid_at,eligible_at)
 SELECT id,'in_test',referrer_uuid,invitee_uuid,'PRO-MONTHLY','xconnect',9,0,'USD',now(),now()+interval '7 days' FROM public.referrals;
INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor)
 SELECT id,'reversal:first','reversal',-1 FROM public.referral_rewards;
DO $$ BEGIN
  BEGIN
    INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor)
    SELECT id,'reversal:second','reversal',-1 FROM public.referral_rewards;
    RAISE EXCEPTION 'duplicate reward event type unexpectedly accepted';
  EXCEPTION WHEN unique_violation THEN NULL;
  END;
END $$;
