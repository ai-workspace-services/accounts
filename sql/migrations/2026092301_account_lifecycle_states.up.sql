-- Additive account lifecycle foundation (P0-1, ai-workspace-services/.github#9).
--
-- Existing users resolve to `active` through the constant column default. This
-- migration does not UPDATE users or alter active, groups, plan, subscription,
-- credentials, or UUID values. Transition metadata remains NULL until the
-- application performs an explicit lifecycle transition.
BEGIN;

ALTER TABLE public.users
  ADD COLUMN IF NOT EXISTS account_lifecycle_state TEXT NOT NULL DEFAULT 'active',
  ADD COLUMN IF NOT EXISTS account_lifecycle_changed_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS account_lifecycle_actor_type TEXT,
  ADD COLUMN IF NOT EXISTS account_lifecycle_actor_ref TEXT,
  ADD COLUMN IF NOT EXISTS account_lifecycle_reason TEXT,
  ADD COLUMN IF NOT EXISTS account_lifecycle_transition_id UUID;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conrelid = 'public.users'::regclass
      AND conname = 'users_account_lifecycle_state_ck'
  ) THEN
    ALTER TABLE public.users
      ADD CONSTRAINT users_account_lifecycle_state_ck
      CHECK (account_lifecycle_state IN (
        'active', 'archived', 'self_cancelled', 'reactivation_pending'
      ));
  END IF;
END;
$$;

CREATE TABLE IF NOT EXISTS public.account_lifecycle_events (
  transition_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  from_state TEXT,
  to_state TEXT NOT NULL,
  actor_type TEXT NOT NULL,
  actor_ref TEXT,
  reason TEXT NOT NULL DEFAULT '',
  request_id TEXT,
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT account_lifecycle_events_from_state_ck CHECK (
    from_state IS NULL OR from_state IN (
      'active', 'archived', 'self_cancelled', 'reactivation_pending'
    )
  ),
  CONSTRAINT account_lifecycle_events_to_state_ck CHECK (
    to_state IN (
      'active', 'archived', 'self_cancelled', 'reactivation_pending'
    )
  ),
  CONSTRAINT account_lifecycle_events_state_change_ck CHECK (
    from_state IS NULL OR from_state <> to_state
  ),
  CONSTRAINT account_lifecycle_events_actor_type_ck CHECK (
    actor_type IN ('user', 'admin', 'system', 'service')
  ),
  CONSTRAINT account_lifecycle_events_metadata_object_ck CHECK (
    jsonb_typeof(metadata) = 'object'
  )
);

ALTER TABLE public.account_lifecycle_events ENABLE ROW LEVEL SECURITY;

CREATE OR REPLACE FUNCTION public.reject_account_lifecycle_event_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'account lifecycle events are immutable'
    USING ERRCODE = '55000';
END;
$$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_trigger
    WHERE tgrelid = 'public.account_lifecycle_events'::regclass
      AND tgname = 'account_lifecycle_events_immutable_trg'
      AND NOT tgisinternal
  ) THEN
    CREATE TRIGGER account_lifecycle_events_immutable_trg
      BEFORE UPDATE OR DELETE ON public.account_lifecycle_events
      FOR EACH ROW
      EXECUTE FUNCTION public.reject_account_lifecycle_event_mutation();
  END IF;
END;
$$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_trigger
    WHERE tgrelid = 'public.account_lifecycle_events'::regclass
      AND tgname = 'account_lifecycle_events_no_truncate_trg'
      AND NOT tgisinternal
  ) THEN
    CREATE TRIGGER account_lifecycle_events_no_truncate_trg
      BEFORE TRUNCATE ON public.account_lifecycle_events
      FOR EACH STATEMENT
      EXECUTE FUNCTION public.reject_account_lifecycle_event_mutation();
  END IF;
END;
$$;

CREATE INDEX IF NOT EXISTS users_account_lifecycle_worklist_idx
  ON public.users (account_lifecycle_state, account_lifecycle_changed_at, uuid)
  WHERE account_lifecycle_state <> 'active';

CREATE INDEX IF NOT EXISTS account_lifecycle_events_user_time_idx
  ON public.account_lifecycle_events (user_uuid, occurred_at DESC);

CREATE INDEX IF NOT EXISTS account_lifecycle_events_actor_time_idx
  ON public.account_lifecycle_events (actor_type, actor_ref, occurred_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS account_lifecycle_events_user_request_uk
  ON public.account_lifecycle_events (user_uuid, request_id)
  WHERE request_id IS NOT NULL;

COMMENT ON COLUMN public.users.account_lifecycle_state IS
  'Account lifecycle state; does not replace users.active or subscription/plan state.';
COMMENT ON COLUMN public.users.account_lifecycle_changed_at IS
  'Time of the latest explicit lifecycle transition; NULL for users without a recorded transition.';
COMMENT ON TABLE public.account_lifecycle_events IS
  'Immutable lifecycle transition audit records. Keep events when an account is archived; user deletion is restricted.';

COMMIT;
