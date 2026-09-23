-- Run only against a new, disposable PostgreSQL database.
\set ON_ERROR_STOP on

DO $$
BEGIN
  IF to_regclass('public.users') IS NOT NULL
     OR to_regclass('public.account_lifecycle_events') IS NOT NULL THEN
    RAISE EXCEPTION 'test requires a fresh database with no public.users or account_lifecycle_events';
  END IF;
END;
$$;

CREATE TABLE public.users (
  uuid UUID PRIMARY KEY,
  username TEXT NOT NULL,
  password TEXT NOT NULL,
  email TEXT,
  level INTEGER NOT NULL,
  groups JSONB NOT NULL,
  active BOOLEAN NOT NULL,
  proxy_uuid UUID NOT NULL,
  subscription_valid_from TIMESTAMPTZ,
  subscription_valid_until TIMESTAMPTZ,
  archived_at TIMESTAMPTZ
);

CREATE TABLE public.subscriptions (
  id BIGINT PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid),
  plan_id TEXT NOT NULL,
  status TEXT NOT NULL,
  external_id TEXT NOT NULL
);

INSERT INTO public.users (
  uuid, username, password, email, level, groups, active, proxy_uuid,
  subscription_valid_from, subscription_valid_until, archived_at
) VALUES (
  '00000000-0000-4000-8000-000000000009', 'legacy-sentinel', 'sentinel-hash',
  'sentinel@example.invalid', 30, '["legacy-group","paid"]'::jsonb, TRUE,
  '00000000-0000-4000-8000-000000000109', '2026-01-01T00:00:00Z',
  '2027-01-01T00:00:00Z', NULL
);

INSERT INTO public.subscriptions (id, user_uuid, plan_id, status, external_id)
VALUES (9, '00000000-0000-4000-8000-000000000009', 'legacy-pro', 'active', 'sentinel-subscription');

CREATE TEMP TABLE lifecycle_user_snapshot AS
SELECT uuid, to_jsonb(users) AS row_data FROM public.users;
CREATE TEMP TABLE lifecycle_subscription_snapshot AS
SELECT id, to_jsonb(subscriptions) AS row_data FROM public.subscriptions;

\ir ../2026092301_account_lifecycle_states.up.sql
\ir ../2026092301_account_lifecycle_states.up.sql

DO $$
DECLARE
  affected BIGINT;
BEGIN
  SELECT count(*) INTO affected
  FROM lifecycle_user_snapshot AS old
  JOIN public.users AS current USING (uuid)
  WHERE old.row_data <> to_jsonb(current) - ARRAY[
    'account_lifecycle_state', 'account_lifecycle_changed_at',
    'account_lifecycle_actor_type', 'account_lifecycle_actor_ref',
    'account_lifecycle_reason', 'account_lifecycle_transition_id'
  ]::TEXT[];
  IF affected <> 0 OR (SELECT count(*) FROM lifecycle_user_snapshot) <> (SELECT count(*) FROM public.users) THEN
    RAISE EXCEPTION 'legacy user row changed during migration';
  END IF;

  SELECT count(*) INTO affected
  FROM lifecycle_subscription_snapshot AS old
  JOIN public.subscriptions AS current USING (id)
  WHERE old.row_data <> to_jsonb(current);
  IF affected <> 0 OR (SELECT count(*) FROM lifecycle_subscription_snapshot) <> (SELECT count(*) FROM public.subscriptions) THEN
    RAISE EXCEPTION 'subscription row changed during migration';
  END IF;

  IF (SELECT account_lifecycle_state FROM public.users WHERE username = 'legacy-sentinel') <> 'active' THEN
    RAISE EXCEPTION 'legacy user did not receive the active lifecycle default';
  END IF;

  BEGIN
    UPDATE public.users SET account_lifecycle_state = 'deleted'
    WHERE username = 'legacy-sentinel';
    RAISE EXCEPTION 'invalid lifecycle state was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;
END;
$$;

INSERT INTO public.account_lifecycle_events (
  user_uuid, from_state, to_state, actor_type, actor_ref, reason, request_id
) VALUES (
  '00000000-0000-4000-8000-000000000009', 'active', 'archived',
  'admin', 'migration-test', 'retention test', 'lifecycle-request-9'
);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM public.account_lifecycle_events
    WHERE user_uuid = '00000000-0000-4000-8000-000000000009'
      AND from_state = 'active'
      AND to_state = 'archived'
      AND actor_type = 'admin'
      AND actor_ref = 'migration-test'
      AND reason = 'retention test'
      AND request_id = 'lifecycle-request-9'
      AND occurred_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'audit event is missing required transition or attribution fields';
  END IF;
END;
$$;

DO $$
BEGIN
  BEGIN
    INSERT INTO public.account_lifecycle_events (
      user_uuid, from_state, to_state, actor_type, reason
    ) VALUES (
      '00000000-0000-4000-8000-000000000009', 'active', 'deleted',
      'admin', 'invalid state'
    );
    RAISE EXCEPTION 'invalid lifecycle event state was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  BEGIN
    INSERT INTO public.account_lifecycle_events (
      user_uuid, from_state, to_state, actor_type, request_id
    ) VALUES (
      '00000000-0000-4000-8000-000000000009', 'active', 'self_cancelled',
      'admin', 'lifecycle-request-9'
    );
    RAISE EXCEPTION 'duplicate request key was accepted';
  EXCEPTION WHEN unique_violation THEN
    NULL;
  END;
END;
$$;

DO $$
DECLARE
  error_message TEXT;
  event_user_fk TEXT;
BEGIN
  BEGIN
    UPDATE public.account_lifecycle_events SET reason = 'changed';
    RAISE EXCEPTION 'audit UPDATE was accepted';
  EXCEPTION WHEN SQLSTATE '55000' THEN
    GET STACKED DIAGNOSTICS error_message = MESSAGE_TEXT;
    IF error_message <> 'account lifecycle events are immutable' THEN
      RAISE EXCEPTION 'audit UPDATE was rejected for an unexpected reason: %', error_message;
    END IF;
  END;

  BEGIN
    DELETE FROM public.account_lifecycle_events;
    RAISE EXCEPTION 'audit DELETE was accepted';
  EXCEPTION WHEN SQLSTATE '55000' THEN
    GET STACKED DIAGNOSTICS error_message = MESSAGE_TEXT;
    IF error_message <> 'account lifecycle events are immutable' THEN
      RAISE EXCEPTION 'audit DELETE was rejected for an unexpected reason: %', error_message;
    END IF;
  END;

  BEGIN
    TRUNCATE public.account_lifecycle_events;
    RAISE EXCEPTION 'audit TRUNCATE was accepted';
  EXCEPTION WHEN SQLSTATE '55000' THEN
    GET STACKED DIAGNOSTICS error_message = MESSAGE_TEXT;
    IF error_message <> 'account lifecycle events are immutable' THEN
      RAISE EXCEPTION 'audit TRUNCATE was rejected for an unexpected reason: %', error_message;
    END IF;
  END;

  SELECT conname INTO event_user_fk
  FROM pg_constraint
  WHERE conrelid = 'public.account_lifecycle_events'::regclass
    AND confrelid = 'public.users'::regclass
    AND contype = 'f'
    AND confdeltype = 'r';
  IF event_user_fk IS NULL THEN
    RAISE EXCEPTION 'audit foreign key does not restrict user deletion';
  END IF;
  EXECUTE format('ALTER TABLE public.account_lifecycle_events DROP CONSTRAINT %I', event_user_fk);

  BEGIN
    DELETE FROM public.users WHERE username = 'legacy-sentinel';
    RAISE EXCEPTION 'hard DELETE of a user was accepted';
  EXCEPTION WHEN SQLSTATE '55000' THEN
    GET STACKED DIAGNOSTICS error_message = MESSAGE_TEXT;
    IF error_message <> 'users are retained; archive the account instead' THEN
      RAISE EXCEPTION 'user DELETE was rejected for an unexpected reason: %', error_message;
    END IF;
  END;

  BEGIN
    TRUNCATE public.users, public.subscriptions;
    RAISE EXCEPTION 'TRUNCATE of users was accepted';
  EXCEPTION WHEN SQLSTATE '55000' THEN
    GET STACKED DIAGNOSTICS error_message = MESSAGE_TEXT;
    IF error_message <> 'users are retained; archive the account instead' THEN
      RAISE EXCEPTION 'user TRUNCATE was rejected for an unexpected reason: %', error_message;
    END IF;
  END;
END;
$$;

CREATE ROLE lifecycle_migration_client NOLOGIN;
GRANT USAGE ON SCHEMA public TO lifecycle_migration_client;
GRANT SELECT ON public.account_lifecycle_events TO lifecycle_migration_client;
SET ROLE lifecycle_migration_client;
DO $$
BEGIN
  IF (SELECT count(*) FROM public.account_lifecycle_events) <> 0 THEN
    RAISE EXCEPTION 'ordinary client role can read lifecycle audit rows';
  END IF;
END;
$$;
RESET ROLE;

REVOKE SELECT ON public.account_lifecycle_events FROM lifecycle_migration_client;
REVOKE USAGE ON SCHEMA public FROM lifecycle_migration_client;
DROP ROLE lifecycle_migration_client;

DO $$
BEGIN
  IF (SELECT count(*) FROM public.account_lifecycle_events) <> 1 THEN
    RAISE EXCEPTION 'audit row was unexpectedly removed';
  END IF;
  IF (SELECT account_lifecycle_state FROM public.users WHERE username = 'legacy-sentinel') <> 'active' THEN
    RAISE EXCEPTION 'user state changed during rejection probes';
  END IF;
END;
$$;

\echo 'PASS: PostgreSQL 17 lifecycle migration rerun, legacy data preservation, state validation, audit immutability/RLS, and user retention'
