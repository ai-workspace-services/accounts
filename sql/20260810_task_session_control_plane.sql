-- Lightweight cross-device TaskThread control plane.
-- Deliberately no artifact table: Bridge/OpenClaw owns task-scoped files and
-- exposes them by task_run_id/bridge_task_ref.

CREATE TABLE IF NOT EXISTS public.task_namespaces (
  id TEXT PRIMARY KEY,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  slug TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  max_active_runs INTEGER NOT NULL DEFAULT 2 CHECK (max_active_runs > 0 AND max_active_runs <= 2),
  last_claimed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (account_uuid, slug)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_namespaces_personal
  ON public.task_namespaces (account_uuid)
  WHERE slug = 'personal';

CREATE TABLE IF NOT EXISTS public.task_sessions (
  id TEXT PRIMARY KEY,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  namespace_id TEXT NOT NULL REFERENCES public.task_namespaces(id) ON DELETE CASCADE,
  title TEXT NOT NULL DEFAULT '',
  snapshot_version BIGINT NOT NULL DEFAULT 0,
  last_event_seq BIGINT NOT NULL DEFAULT 0,
  lifecycle_state TEXT NOT NULL DEFAULT 'ready',
  context_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (jsonb_typeof(context_summary) = 'object'),
  CHECK (octet_length(context_summary::text) <= 131072)
);

CREATE INDEX IF NOT EXISTS idx_task_sessions_namespace_updated
  ON public.task_sessions (namespace_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS public.task_session_events (
  session_id TEXT NOT NULL REFERENCES public.task_sessions(id) ON DELETE CASCADE,
  seq BIGINT NOT NULL,
  event_type TEXT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  actor_id TEXT NOT NULL DEFAULT '',
  client_request_id TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, seq),
  CHECK (jsonb_typeof(payload) = 'object'),
  CHECK (octet_length(payload::text) <= 16384)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_session_events_client_request
  ON public.task_session_events (session_id, client_request_id)
  WHERE client_request_id <> '';

CREATE INDEX IF NOT EXISTS idx_task_session_events_created
  ON public.task_session_events (session_id, created_at DESC);

CREATE TABLE IF NOT EXISTS public.task_runs (
  id TEXT PRIMARY KEY,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  namespace_id TEXT NOT NULL REFERENCES public.task_namespaces(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL REFERENCES public.task_sessions(id) ON DELETE CASCADE,
  client_request_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'queued',
  priority INTEGER NOT NULL DEFAULT 0,
  not_before TIMESTAMPTZ NOT NULL DEFAULT now(),
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_token_hash TEXT NOT NULL DEFAULT '',
  lease_expires_at TIMESTAMPTZ,
  fence BIGINT NOT NULL DEFAULT 0,
  routing JSONB NOT NULL DEFAULT '{}'::jsonb,
  bridge_task_ref TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (state IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
  CHECK (jsonb_typeof(routing) = 'object')
);

CREATE INDEX IF NOT EXISTS idx_task_runs_claimable
  ON public.task_runs (account_uuid, state, not_before, priority DESC, created_at);

CREATE INDEX IF NOT EXISTS idx_task_runs_namespace_state
  ON public.task_runs (namespace_id, state, updated_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_runs_client_request
  ON public.task_runs (session_id, client_request_id)
  WHERE client_request_id <> '';
