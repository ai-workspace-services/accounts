\set ON_ERROR_STOP on
CREATE TABLE public.users (uuid uuid PRIMARY KEY, email text NOT NULL UNIQUE);
CREATE TABLE public.overlay_devices (user_uuid uuid NOT NULL REFERENCES users(uuid), id text NOT NULL, PRIMARY KEY(user_uuid,id));
INSERT INTO users VALUES ('00000000-0000-4000-8000-000000000001','ci@example.invalid');
INSERT INTO overlay_devices VALUES ('00000000-0000-4000-8000-000000000001','ci-device');
CREATE TEMP TABLE user_before AS SELECT md5(to_jsonb(t)::text) digest FROM users t;
BEGIN;
\ir ../2026100601_full_business_legacy_compat.up.sql
COMMIT;
INSERT INTO overlay_nodes(id,wireguard_public_key,wireguard_address,endpoint_host) VALUES ('ci-node','ci-key','10.1.0.1','ci.invalid');
INSERT INTO overlay_config_acks(user_uuid,device_id,revision,applied_at) VALUES ('00000000-0000-4000-8000-000000000001','ci-device','ci',now());
CREATE TEMP TABLE nodes_before AS SELECT md5(to_jsonb(t)::text) digest FROM overlay_nodes t;
CREATE TEMP TABLE acks_before AS SELECT md5(to_jsonb(t)::text) digest FROM overlay_config_acks t;
BEGIN;
\ir ../2026100601_full_business_legacy_compat.up.sql
COMMIT;
DO $$ BEGIN
 IF (SELECT array_agg(digest ORDER BY digest) FROM user_before) IS DISTINCT FROM
 (SELECT array_agg(md5(to_jsonb(t)::text) ORDER BY md5(to_jsonb(t)::text)) FROM users t)
 OR (SELECT array_agg(digest ORDER BY digest) FROM nodes_before) IS DISTINCT FROM
 (SELECT array_agg(md5(to_jsonb(t)::text) ORDER BY md5(to_jsonb(t)::text)) FROM overlay_nodes t)
 OR (SELECT array_agg(digest ORDER BY digest) FROM acks_before) IS DISTINCT FROM
 (SELECT array_agg(md5(to_jsonb(t)::text) ORDER BY md5(to_jsonb(t)::text)) FROM overlay_config_acks t) THEN
  RAISE EXCEPTION 'Existing business rows changed';
 END IF;
 IF (SELECT count(*) FROM pg_constraint WHERE conrelid='overlay_config_acks'::regclass AND contype='f') <> 2 THEN
  RAISE EXCEPTION 'Full identity/device FK contract missing';
 END IF;
 IF EXISTS (SELECT 1 FROM pg_class WHERE oid IN ('overlay_nodes'::regclass,'overlay_config_acks'::regclass) AND NOT relrowsecurity) THEN
  RAISE EXCEPTION 'Private legacy compatibility tables require RLS';
 END IF;
END $$;
