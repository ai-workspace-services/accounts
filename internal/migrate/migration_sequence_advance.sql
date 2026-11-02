-- Owner-operated target-only installation. The caller must bind target physical
-- identity in the session and independently verify the backup and schema.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';
DO $guard$
DECLARE executor_oid oid; owner_oid oid; existing oid;
BEGIN
  IF current_setting('accounts_migration_guard.system_identifier',true) IS DISTINCT FROM (SELECT system_identifier::text FROM pg_control_system())
     OR current_setting('accounts_migration_guard.database_oid',true) IS DISTINCT FROM (SELECT oid::text FROM pg_database WHERE datname=current_database()) THEN
    RAISE EXCEPTION 'target physical identity not bound';
  END IF;
  SELECT oid INTO executor_oid FROM pg_roles WHERE rolname='accounts_migration' AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit;
  SELECT oid INTO owner_oid FROM pg_roles WHERE rolname=current_user;
  IF executor_oid IS NULL OR executor_oid=owner_oid OR EXISTS (SELECT 1 FROM pg_auth_members WHERE roleid=executor_oid OR member=executor_oid)
     OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relowner=executor_oid) THEN
    RAISE EXCEPTION 'migration login absent, privileged, assumable, or owns business tables';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname='accounts_migration_api' AND n.nspowner=owner_oid
    AND NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(n.nspacl,acldefault('n',n.nspowner))) a WHERE a.privilege_type='CREATE' AND a.grantee<>owner_oid)) THEN
    RAISE EXCEPTION 'migration API schema must be protected and owned by installer';
  END IF;
  IF EXISTS (SELECT 1 FROM (VALUES ('finance_operation_events','id','public.finance_operation_events_id_seq'),('referral_campaign_revisions','revision','public.referral_campaign_revisions_revision_seq')) v(tab,col,seq)
    WHERE pg_get_serial_sequence('public.'||v.tab,v.col) IS DISTINCT FROM v.seq
       OR NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_sequence s ON s.seqrelid=c.oid WHERE c.oid=to_regclass(v.seq) AND c.relkind='S' AND c.relowner=owner_oid AND s.seqincrement=1 AND s.seqcache=1 AND NOT s.seqcycle)) THEN
    RAISE EXCEPTION 'reviewed sequence ownership or configuration differs';
  END IF;
  SELECT p.oid INTO existing FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='accounts_migration_api' AND p.proname='advance_sequences' AND p.pronargs=0;
  IF existing IS NOT NULL AND NOT EXISTS (SELECT 1 FROM pg_proc p WHERE p.oid=existing AND p.proowner=owner_oid AND p.prosecdef AND encode(sha256(convert_to(p.prosrc,'UTF8')),'hex')='8f1f44cf372ee1f34a1e30e7d0c6df0e0eaa4388082f64178959506dfd92ca05') THEN
    RAISE EXCEPTION 'unreviewed sequence function already exists';
  END IF;
END
$guard$;

CREATE OR REPLACE FUNCTION accounts_migration_api.advance_sequences() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $body$
DECLARE n bigint;
BEGIN
  IF session_user <> 'accounts_migration' OR NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=session_user AND rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit)
     OR EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.rolname=session_user WHERE m.roleid=r.oid OR m.member=r.oid)
     OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace ns ON ns.oid=c.relnamespace JOIN pg_roles r ON r.oid=c.relowner WHERE ns.nspname='public' AND c.relkind IN ('r','p') AND r.rolname=session_user) THEN
    RAISE EXCEPTION 'migration identity is privileged or assumable';
  END IF;
  IF pg_get_serial_sequence('public.finance_operation_events','id') IS DISTINCT FROM 'public.finance_operation_events_id_seq'
     OR pg_get_serial_sequence('public.referral_campaign_revisions','revision') IS DISTINCT FROM 'public.referral_campaign_revisions_revision_seq'
     OR EXISTS (SELECT 1 FROM (VALUES ('public.finance_operation_events_id_seq'),('public.referral_campaign_revisions_revision_seq')) v(seq)
       WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_sequence s ON s.seqrelid=c.oid WHERE c.oid=to_regclass(v.seq) AND c.relowner=(SELECT oid FROM pg_roles WHERE rolname=current_user) AND s.seqincrement=1 AND s.seqcache=1 AND NOT s.seqcycle))
     OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_roles r ON r.rolname=current_user WHERE c.oid IN ('public.finance_operation_events'::regclass,'public.referral_campaign_revisions'::regclass) AND c.relrowsecurity AND NOT (r.rolsuper OR r.rolbypassrls OR (c.relowner=r.oid AND NOT c.relforcerowsecurity))) THEN
    RAISE EXCEPTION 'reviewed sequence ownership or configuration differs';
  END IF;
  -- The no-op ALTER takes each sequence's transactional lock before reading
  -- last_value, preventing concurrent nextval from being moved backwards.
  ALTER SEQUENCE public.finance_operation_events_id_seq INCREMENT BY 1;
  SELECT GREATEST(COALESCE(max(id),0)+1,(SELECT last_value+1 FROM public.finance_operation_events_id_seq)) INTO n FROM public.finance_operation_events;
  EXECUTE format('ALTER SEQUENCE public.finance_operation_events_id_seq RESTART WITH %s',n);
  ALTER SEQUENCE public.referral_campaign_revisions_revision_seq INCREMENT BY 1;
  SELECT GREATEST(COALESCE(max(revision),0)+1,(SELECT last_value+1 FROM public.referral_campaign_revisions_revision_seq)) INTO n FROM public.referral_campaign_revisions;
  EXECUTE format('ALTER SEQUENCE public.referral_campaign_revisions_revision_seq RESTART WITH %s',n);
END;
$body$;
REVOKE ALL ON FUNCTION accounts_migration_api.advance_sequences() FROM PUBLIC;
GRANT USAGE ON SCHEMA accounts_migration_api TO accounts_migration;
GRANT EXECUTE ON FUNCTION accounts_migration_api.advance_sequences() TO accounts_migration;
COMMIT;
