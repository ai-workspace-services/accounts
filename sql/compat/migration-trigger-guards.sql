-- Target-side compatibility for exact source-row catch-up. This file is an
-- owner-operated schema change; it must never run on the source database.
-- Install only after target backup/restore, role ownership and schema review.
-- The dedicated login is not a business or audit identity. No role may be
-- granted membership in it; superusers retain their inherent authority.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

DO $guard$
DECLARE
  executor_oid oid;
  fname text;
  body text;
  original text;
  replacement text;
BEGIN
  -- The operator must independently resolve the physical cluster and DB OID
  -- and set both session values before executing this file. No source default.
  IF current_setting('accounts_migration_guard.system_identifier', true) IS DISTINCT FROM (SELECT system_identifier::text FROM pg_control_system())
     OR current_setting('accounts_migration_guard.database_oid', true) IS DISTINCT FROM (SELECT oid::text FROM pg_database WHERE datname=current_database()) THEN
    RAISE EXCEPTION 'target physical identity not bound';
  END IF;
  SELECT oid INTO executor_oid FROM pg_roles
  WHERE rolname = 'accounts_migration' AND rolcanlogin
    AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
    AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit;
  IF executor_oid IS NULL OR EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid = executor_oid OR member = executor_oid)
     OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relowner=executor_oid) THEN
    RAISE EXCEPTION 'dedicated migration login absent, privileged, assumable, or owns business tables';
  END IF;
  FOREACH fname IN ARRAY ARRAY['set_updated_at', 'bump_version'] LOOP
    SELECT p.prosrc INTO body FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_language l ON l.oid=p.prolang
      WHERE n.nspname='public' AND p.proname=fname AND p.pronargs=0
        AND p.prorettype='pg_catalog.trigger'::regtype AND NOT p.prosecdef
        AND l.lanname='plpgsql' AND p.prokind='f' AND p.provolatile='v' AND p.proparallel='u'
        AND NOT p.proisstrict AND NOT p.proleakproof AND p.proconfig IS NULL
        AND p.proowner <> executor_oid;
    IF fname = 'set_updated_at' THEN
      original := 'BEGIN NEW.updated_at := now(); RETURN NEW; END;';
      replacement := $expected$
BEGIN
  IF current_user = 'accounts_migration' THEN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit)
       OR EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.rolname=current_user WHERE m.roleid=r.oid OR m.member=r.oid)
       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE n.nspname='public' AND c.relkind IN ('r','p') AND r.rolname=current_user) THEN
      RAISE EXCEPTION 'migration identity is privileged or assumable';
    END IF;
    RETURN NEW;
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END;
$expected$;
    ELSE
      original := 'BEGIN IF TG_OP = ''UPDATE'' THEN NEW.version := COALESCE(OLD.version, 0) + 1; END IF; RETURN NEW; END;';
      replacement := $expected$
BEGIN
  IF current_user = 'accounts_migration' THEN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit)
       OR EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.rolname=current_user WHERE m.roleid=r.oid OR m.member=r.oid)
       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE n.nspname='public' AND c.relkind IN ('r','p') AND r.rolname=current_user) THEN
      RAISE EXCEPTION 'migration identity is privileged or assumable';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' THEN NEW.version := COALESCE(OLD.version, 0) + 1; END IF;
  RETURN NEW;
END;
$expected$;
    END IF;
    IF body IS NULL OR btrim(regexp_replace(body, '\s+', ' ', 'g')) NOT IN (original, btrim(regexp_replace(replacement, '\s+', ' ', 'g'))) THEN
      RAISE EXCEPTION 'unrecognized trigger function: %', fname;
    END IF;
  END LOOP;
END
$guard$;

CREATE OR REPLACE FUNCTION public.set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $body$
BEGIN
  IF current_user = 'accounts_migration' THEN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit)
       OR EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.rolname=current_user WHERE m.roleid=r.oid OR m.member=r.oid)
       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE n.nspname='public' AND c.relkind IN ('r','p') AND r.rolname=current_user) THEN
      RAISE EXCEPTION 'migration identity is privileged or assumable';
    END IF;
    RETURN NEW;
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END;
$body$;

CREATE OR REPLACE FUNCTION public.bump_version() RETURNS trigger LANGUAGE plpgsql AS $body$
BEGIN
  IF current_user = 'accounts_migration' THEN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit)
       OR EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.rolname=current_user WHERE m.roleid=r.oid OR m.member=r.oid)
       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE n.nspname='public' AND c.relkind IN ('r','p') AND r.rolname=current_user) THEN
      RAISE EXCEPTION 'migration identity is privileged or assumable';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' THEN NEW.version := COALESCE(OLD.version, 0) + 1; END IF;
  RETURN NEW;
END;
$body$;
COMMIT;
