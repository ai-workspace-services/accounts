\set ON_ERROR_STOP on
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE public.users (uuid UUID PRIMARY KEY DEFAULT gen_random_uuid());
CREATE TABLE public.subscriptions (uuid UUID PRIMARY KEY DEFAULT gen_random_uuid());
CREATE TABLE public.finance_invoices (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), idempotency_key TEXT NOT NULL UNIQUE, account_uuid UUID NOT NULL, subscription_uuid UUID NOT NULL, provider TEXT NOT NULL, provider_invoice_id TEXT NOT NULL UNIQUE, amount_minor BIGINT NOT NULL, currency TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', issued_at TIMESTAMPTZ NOT NULL DEFAULT now(), due_at TIMESTAMPTZ NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
\ir ../2026101301_referral_program.up.sql
\ir ../2026101301_referral_program.down.sql
DO $$ BEGIN
 IF to_regclass('public.referral_rewards') IS NOT NULL THEN RAISE EXCEPTION 'down migration left referral tables behind'; END IF;
END $$;
