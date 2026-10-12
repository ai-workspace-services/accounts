-- Additive only: no rewrite of users, credentials, subscriptions or ledgers.
BEGIN;
CREATE TABLE IF NOT EXISTS public.network_teams (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  member_limit INTEGER NOT NULL CHECK (member_limit > 0),
  valid_from TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ NOT NULL,
  active BOOLEAN NOT NULL DEFAULT FALSE,
  CHECK (valid_until > valid_from)
);
CREATE TABLE IF NOT EXISTS public.network_team_members (
  user_uuid UUID PRIMARY KEY REFERENCES public.users(uuid) ON DELETE RESTRICT,
  team_id TEXT REFERENCES public.network_teams(id) ON DELETE RESTRICT,
  dedicated_only BOOLEAN NOT NULL DEFAULT TRUE,
  CHECK (team_id IS NULL OR dedicated_only)
);
CREATE INDEX IF NOT EXISTS network_team_members_team_idx ON public.network_team_members(team_id);
ALTER TABLE public.network_teams ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.network_team_members ENABLE ROW LEVEL SECURITY;
COMMIT;
