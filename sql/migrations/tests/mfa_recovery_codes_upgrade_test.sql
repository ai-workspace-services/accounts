-- Run only against a new, disposable PostgreSQL database.
\set ON_ERROR_STOP on

DO $$
BEGIN
  IF to_regclass('public.users') IS NOT NULL
     OR to_regclass('public.sessions') IS NOT NULL
     OR to_regclass('public.subscriptions') IS NOT NULL THEN
    RAISE EXCEPTION 'test requires a fresh database without account tables';
  END IF;
END;
$$;

CREATE TABLE public.users (
  uuid UUID PRIMARY KEY,
  username TEXT NOT NULL,
  password TEXT NOT NULL,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  archived_at TIMESTAMPTZ
);
CREATE TABLE public.sessions (
  token TEXT PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid),
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE public.subscriptions (id UUID PRIMARY KEY, user_uuid UUID NOT NULL, status TEXT NOT NULL);
CREATE TABLE public.billing_ledger (id UUID PRIMARY KEY, account_uuid UUID NOT NULL, amount BIGINT NOT NULL);
CREATE TABLE public.refunds (id UUID PRIMARY KEY, account_uuid UUID NOT NULL, amount BIGINT NOT NULL);
CREATE TABLE public.usage_ledger (id UUID PRIMARY KEY, account_uuid UUID NOT NULL, bytes BIGINT NOT NULL);

INSERT INTO public.users (uuid, username, password)
VALUES ('00000000-0000-4000-8000-000000000004', 'p0-4-sentinel', 'sentinel-hash');
INSERT INTO public.sessions (token, user_uuid, expires_at)
VALUES ('p0-4-sentinel-session', '00000000-0000-4000-8000-000000000004', now() + interval '1 day');
INSERT INTO public.subscriptions VALUES ('00000000-0000-4000-8000-000000000104', '00000000-0000-4000-8000-000000000004', 'active');
INSERT INTO public.billing_ledger VALUES ('00000000-0000-4000-8000-000000000204', '00000000-0000-4000-8000-000000000004', 100);
INSERT INTO public.refunds VALUES ('00000000-0000-4000-8000-000000000304', '00000000-0000-4000-8000-000000000004', 10);
INSERT INTO public.usage_ledger VALUES ('00000000-0000-4000-8000-000000000404', '00000000-0000-4000-8000-000000000004', 5000);

CREATE TEMP TABLE p0_4_users AS SELECT uuid, to_jsonb(users) AS row_data FROM public.users;
CREATE TEMP TABLE p0_4_sessions AS SELECT token, to_jsonb(sessions) AS row_data FROM public.sessions;
CREATE TEMP TABLE p0_4_subscriptions AS SELECT id, to_jsonb(subscriptions) AS row_data FROM public.subscriptions;
CREATE TEMP TABLE p0_4_payments AS SELECT id, to_jsonb(billing_ledger) AS row_data FROM public.billing_ledger;
CREATE TEMP TABLE p0_4_refunds AS SELECT id, to_jsonb(refunds) AS row_data FROM public.refunds;
CREATE TEMP TABLE p0_4_usage AS SELECT id, to_jsonb(usage_ledger) AS row_data FROM public.usage_ledger;

\ir ../2026092703_mfa_recovery_codes.up.sql
\ir ../2026092703_mfa_recovery_codes.up.sql

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM p0_4_users old JOIN public.users current USING (uuid) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM p0_4_users) <> (SELECT count(*) FROM public.users) THEN
    RAISE EXCEPTION 'migration changed or removed users';
  END IF;
  IF EXISTS (SELECT 1 FROM p0_4_sessions old JOIN public.sessions current USING (token) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM p0_4_sessions) <> (SELECT count(*) FROM public.sessions) THEN
    RAISE EXCEPTION 'migration changed or removed sessions';
  END IF;
  IF EXISTS (SELECT 1 FROM p0_4_subscriptions old JOIN public.subscriptions current USING (id) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM p0_4_subscriptions) <> (SELECT count(*) FROM public.subscriptions) THEN
    RAISE EXCEPTION 'migration changed or removed subscriptions';
  END IF;
  IF EXISTS (SELECT 1 FROM p0_4_payments old JOIN public.billing_ledger current USING (id) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM p0_4_payments) <> (SELECT count(*) FROM public.billing_ledger) THEN
    RAISE EXCEPTION 'migration changed or removed payments';
  END IF;
  IF EXISTS (SELECT 1 FROM p0_4_refunds old JOIN public.refunds current USING (id) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM p0_4_refunds) <> (SELECT count(*) FROM public.refunds) THEN
    RAISE EXCEPTION 'migration changed or removed refunds';
  END IF;
  IF EXISTS (SELECT 1 FROM p0_4_usage old JOIN public.usage_ledger current USING (id) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM p0_4_usage) <> (SELECT count(*) FROM public.usage_ledger) THEN
    RAISE EXCEPTION 'migration changed or removed usage records';
  END IF;
  IF (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'mfa_recovery_codes') <> 1 THEN
    RAISE EXCEPTION 'recovery code table was not created exactly once';
  END IF;
END;
$$;

INSERT INTO public.mfa_recovery_codes (id, user_uuid, batch_uuid, code_hash, expires_at)
VALUES ('00000000-0000-4000-8000-000000000504', '00000000-0000-4000-8000-000000000004',
        '00000000-0000-4000-8000-000000000604', 'bcrypt-hash-only', now() + interval '365 days');

DO $$
BEGIN
  BEGIN
    INSERT INTO public.mfa_recovery_codes (id, user_uuid, batch_uuid, code_hash, expires_at, failed_attempts)
    VALUES ('00000000-0000-4000-8000-000000000505', '00000000-0000-4000-8000-000000000004',
            '00000000-0000-4000-8000-000000000604', 'invalid', now() + interval '1 day', -1);
    RAISE EXCEPTION 'negative failure count was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;
END;
$$;
