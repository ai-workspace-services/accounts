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
  email TEXT,
  email_verified_at TIMESTAMPTZ,
  groups JSONB NOT NULL DEFAULT '[]'::jsonb,
  active BOOLEAN NOT NULL DEFAULT TRUE
);
CREATE TABLE public.sessions (
  token TEXT PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid),
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE public.subscriptions (
  id BIGINT PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid),
  plan_id TEXT NOT NULL,
  status TEXT NOT NULL
);
CREATE TABLE public.billing_ledger (
  id UUID PRIMARY KEY,
  account_uuid UUID NOT NULL,
  entry_type TEXT NOT NULL,
  rated_bytes BIGINT NOT NULL
);
CREATE TABLE public.traffic_minute_buckets (
  bucket_start TIMESTAMPTZ NOT NULL,
  node_id TEXT NOT NULL,
  account_uuid UUID NOT NULL,
  total_bytes BIGINT NOT NULL,
  PRIMARY KEY (bucket_start, node_id, account_uuid)
);
CREATE TABLE public.stripe_webhook_events (
  event_id TEXT PRIMARY KEY,
  event_type TEXT NOT NULL,
  payload JSONB NOT NULL
);

INSERT INTO public.users (uuid, username, password, email, email_verified_at, groups, active)
VALUES ('00000000-0000-4000-8000-000000000002', 'p0-2-sentinel', 'sentinel-hash',
        'p0-2@example.invalid', '2026-01-01T00:00:00Z', '["existing-group"]', TRUE);
INSERT INTO public.sessions (token, user_uuid, expires_at)
VALUES ('sentinel-session', '00000000-0000-4000-8000-000000000002', now() + interval '1 day');
INSERT INTO public.subscriptions (id, user_uuid, plan_id, status)
VALUES (2, '00000000-0000-4000-8000-000000000002', 'legacy-plan', 'active');
INSERT INTO public.billing_ledger (id, account_uuid, entry_type, rated_bytes)
VALUES ('00000000-0000-4000-8000-000000000302', '00000000-0000-4000-8000-000000000002', 'usage', 12345);
INSERT INTO public.traffic_minute_buckets (bucket_start, node_id, account_uuid, total_bytes)
VALUES ('2026-01-01T00:00:00Z', 'legacy-node', '00000000-0000-4000-8000-000000000002', 12345);
INSERT INTO public.stripe_webhook_events (event_id, event_type, payload)
VALUES ('evt_p0_2_sentinel', 'invoice.paid', '{"sentinel":true}');

CREATE TEMP TABLE p0_2_user_snapshot AS SELECT uuid, to_jsonb(users) AS row_data FROM public.users;
CREATE TEMP TABLE p0_2_session_snapshot AS SELECT token, to_jsonb(sessions) AS row_data FROM public.sessions;
CREATE TEMP TABLE p0_2_subscription_snapshot AS SELECT id, to_jsonb(subscriptions) AS row_data FROM public.subscriptions;
CREATE TEMP TABLE p0_2_billing_snapshot AS SELECT id, to_jsonb(billing_ledger) AS row_data FROM public.billing_ledger;
CREATE TEMP TABLE p0_2_usage_snapshot AS SELECT bucket_start, node_id, account_uuid, to_jsonb(traffic_minute_buckets) AS row_data FROM public.traffic_minute_buckets;
CREATE TEMP TABLE p0_2_payment_snapshot AS SELECT event_id, to_jsonb(stripe_webhook_events) AS row_data FROM public.stripe_webhook_events;

\ir ../2026092702_password_recovery_challenges.up.sql
\ir ../2026092702_password_recovery_challenges.up.sql

DO $$
DECLARE changed_rows BIGINT;
BEGIN
  SELECT count(*) INTO changed_rows FROM p0_2_user_snapshot old
  JOIN public.users current USING (uuid) WHERE old.row_data <> to_jsonb(current);
  IF changed_rows <> 0 OR (SELECT count(*) FROM p0_2_user_snapshot) <> (SELECT count(*) FROM public.users) THEN
    RAISE EXCEPTION 'migration changed existing user data';
  END IF;
  SELECT count(*) INTO changed_rows FROM p0_2_session_snapshot old
  JOIN public.sessions current USING (token) WHERE old.row_data <> to_jsonb(current);
  IF changed_rows <> 0 OR (SELECT count(*) FROM p0_2_session_snapshot) <> (SELECT count(*) FROM public.sessions) THEN
    RAISE EXCEPTION 'migration changed existing sessions';
  END IF;
  SELECT count(*) INTO changed_rows FROM p0_2_subscription_snapshot old
  JOIN public.subscriptions current USING (id) WHERE old.row_data <> to_jsonb(current);
  IF changed_rows <> 0 OR (SELECT count(*) FROM p0_2_subscription_snapshot) <> (SELECT count(*) FROM public.subscriptions) THEN
    RAISE EXCEPTION 'migration changed existing subscriptions';
  END IF;
  SELECT count(*) INTO changed_rows FROM p0_2_billing_snapshot old
  JOIN public.billing_ledger current USING (id) WHERE old.row_data <> to_jsonb(current);
  IF changed_rows <> 0 OR (SELECT count(*) FROM p0_2_billing_snapshot) <> (SELECT count(*) FROM public.billing_ledger) THEN
    RAISE EXCEPTION 'migration changed billing ledger records';
  END IF;
  SELECT count(*) INTO changed_rows FROM p0_2_usage_snapshot old
  JOIN public.traffic_minute_buckets current USING (bucket_start, node_id, account_uuid)
  WHERE old.row_data <> to_jsonb(current);
  IF changed_rows <> 0 OR (SELECT count(*) FROM p0_2_usage_snapshot) <> (SELECT count(*) FROM public.traffic_minute_buckets) THEN
    RAISE EXCEPTION 'migration changed usage records';
  END IF;
  SELECT count(*) INTO changed_rows FROM p0_2_payment_snapshot old
  JOIN public.stripe_webhook_events current USING (event_id) WHERE old.row_data <> to_jsonb(current);
  IF changed_rows <> 0 OR (SELECT count(*) FROM p0_2_payment_snapshot) <> (SELECT count(*) FROM public.stripe_webhook_events) THEN
    RAISE EXCEPTION 'migration changed payment webhook records';
  END IF;
  IF (SELECT count(*) FROM information_schema.tables
      WHERE table_schema = 'public' AND table_name = 'password_recovery_challenges') <> 1 THEN
    RAISE EXCEPTION 'password recovery challenge table was not created exactly once';
  END IF;
END;
$$;

INSERT INTO public.password_recovery_challenges
(id, user_uuid, email_snapshot, challenge_kind, secret_hash, expires_at)
VALUES ('00000000-0000-4000-8000-000000000202', '00000000-0000-4000-8000-000000000002',
        'p0-2@example.invalid', 'token', 'sha256-hash-only', now() + interval '30 minutes');

DO $$
BEGIN
  BEGIN
    INSERT INTO public.password_recovery_challenges
    (id, user_uuid, email_snapshot, challenge_kind, secret_hash, expires_at)
    VALUES ('00000000-0000-4000-8000-000000000203', '00000000-0000-4000-8000-000000000002',
            'p0-2@example.invalid', 'token', 'another-hash', now() + interval '30 minutes');
    RAISE EXCEPTION 'multiple active challenges of the same kind were accepted';
  EXCEPTION WHEN unique_violation THEN
    NULL;
  END;
  BEGIN
    INSERT INTO public.password_recovery_challenges
    (id, user_uuid, email_snapshot, challenge_kind, secret_hash, expires_at)
    VALUES ('00000000-0000-4000-8000-000000000204', '00000000-0000-4000-8000-000000000002',
            'p0-2@example.invalid', 'unknown', 'invalid-hash', now() + interval '30 minutes');
    RAISE EXCEPTION 'invalid challenge kind was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;
END;
$$;
