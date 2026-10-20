-- Forward-only user-level state. Never reset this row on subscription changes.
CREATE TABLE public.subscription_lifecycle_states (
  account_uuid uuid PRIMARY KEY REFERENCES public.users(uuid),
  version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
  last_change_at timestamptz,
  operation_id uuid REFERENCES public.finance_operations(id),
  pending_plan_id text NOT NULL DEFAULT '',
  effective_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((pending_plan_id = '') = (effective_at IS NULL))
);
CREATE INDEX subscription_lifecycle_due_idx
  ON public.subscription_lifecycle_states(effective_at)
  WHERE effective_at IS NOT NULL;

ALTER TABLE public.subscription_lifecycle_states ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON public.subscription_lifecycle_states FROM PUBLIC;
DO $$
DECLARE client_role text;
BEGIN
  FOREACH client_role IN ARRAY ARRAY['anon','authenticated'] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname=client_role) THEN
      EXECUTE format('REVOKE ALL ON public.subscription_lifecycle_states FROM %I',client_role);
    END IF;
  END LOOP;
END;
$$;
