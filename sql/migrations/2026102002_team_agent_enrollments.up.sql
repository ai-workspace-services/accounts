-- Node claims are inventory only. Only an audited administrator decision grants access.
CREATE TABLE public.team_agent_enrollments (
  agent_id text PRIMARY KEY,
  suggested_team_id text NOT NULL DEFAULT '',
  assigned_team_id text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','revoked')),
  decided_by text NOT NULL DEFAULT '',
  reason text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE public.team_agent_enrollments ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON public.team_agent_enrollments FROM PUBLIC;
