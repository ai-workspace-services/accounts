-- Run only in a fresh disposable PostgreSQL database.
\set ON_ERROR_STOP on
DO $$ BEGIN
  IF to_regclass('public.users') IS NOT NULL THEN
    RAISE EXCEPTION 'network team migration test requires a fresh database';
  END IF;
END $$;
CREATE TABLE public.users(uuid UUID PRIMARY KEY, username TEXT NOT NULL, payload JSONB NOT NULL);
INSERT INTO public.users VALUES('11111111-1111-4111-8111-111111111111','sentinel','{"credential":"test-only","history":[1,2,3]}');
CREATE TEMP TABLE users_before AS SELECT * FROM public.users;
\ir ../2026101201_network_teams.up.sql
\ir ../2026101201_network_teams.up.sql
DO $$ BEGIN
  IF EXISTS ((SELECT * FROM public.users EXCEPT SELECT * FROM users_before)
     UNION ALL (SELECT * FROM users_before EXCEPT SELECT * FROM public.users)) THEN
    RAISE EXCEPTION 'migration modified existing user rows';
  END IF;
  IF EXISTS(SELECT 1 FROM pg_class WHERE relname IN ('network_teams','network_team_members') AND NOT relrowsecurity) THEN
    RAISE EXCEPTION 'network team metadata must deny unprivileged direct SQL reads';
  END IF;
  IF EXISTS(SELECT 1 FROM public.network_team_members) THEN
    RAISE EXCEPTION 'migration assigned existing personal users';
  END IF;
END $$;
INSERT INTO public.network_teams VALUES('acme','ACME',1,now(),now()+interval '1 month',true);
INSERT INTO public.network_team_members VALUES('11111111-1111-4111-8111-111111111111','acme',true);
UPDATE public.network_team_members SET team_id=NULL WHERE user_uuid='11111111-1111-4111-8111-111111111111';
DO $$ BEGIN
  IF NOT (SELECT dedicated_only FROM public.network_team_members WHERE user_uuid='11111111-1111-4111-8111-111111111111') THEN
    RAISE EXCEPTION 'member removal restored public access';
  END IF;
END $$;
