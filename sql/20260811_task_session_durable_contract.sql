-- Durable/idempotent command additions for the lightweight task-session
-- control plane. Bridge/OpenClaw remains the owner of artifacts and task file
-- contents; this migration stores only event JSON, state and opaque refs.

ALTER TABLE public.task_runs
  ADD COLUMN IF NOT EXISTS client_request_id TEXT NOT NULL DEFAULT '';

ALTER TABLE public.task_namespaces
  ADD COLUMN IF NOT EXISTS last_claimed_at TIMESTAMPTZ;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'task_namespaces_max_active_runs_mvp_ck'
      AND conrelid = 'public.task_namespaces'::regclass
  ) THEN
    ALTER TABLE public.task_namespaces
      ADD CONSTRAINT task_namespaces_max_active_runs_mvp_ck
      CHECK (max_active_runs > 0 AND max_active_runs <= 2) NOT VALID;
  END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_runs_client_request
  ON public.task_runs (session_id, client_request_id)
  WHERE client_request_id <> '';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'task_sessions_context_summary_size_ck'
      AND conrelid = 'public.task_sessions'::regclass
  ) THEN
    ALTER TABLE public.task_sessions
      ADD CONSTRAINT task_sessions_context_summary_size_ck
      CHECK (octet_length(context_summary::text) <= 131072) NOT VALID;
  END IF;
END
$$;
