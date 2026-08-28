-- XConnect-One NetworkPolicy v1alpha1 source/build/activation history.
BEGIN;
CREATE TABLE public.overlay_policy_revisions (
  network_id TEXT NOT NULL,
  revision BIGINT NOT NULL CHECK (revision>0),
  owner_user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (btrim(name)<>''),
  source JSONB NOT NULL,
  artifact JSONB NOT NULL,
  artifact_sha256 TEXT NOT NULL CHECK (artifact_sha256~'^[a-f0-9]{64}$'),
  compiler_version TEXT NOT NULL CHECK (btrim(compiler_version)<>''),
  warnings JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(warnings)='array'),
  status TEXT NOT NULL CHECK (status IN ('draft','active','superseded')),
  generation BIGINT NOT NULL CHECK (generation>=0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  validated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  activated_at TIMESTAMPTZ,
  PRIMARY KEY(network_id,revision),
  CONSTRAINT overlay_policy_revision_state_ck CHECK ((status='draft' AND generation=0 AND activated_at IS NULL) OR (status IN ('active','superseded') AND generation>0 AND activated_at IS NOT NULL)),
  CONSTRAINT overlay_policy_revision_artifact_ck CHECK (artifact->>'compiler_version'=compiler_version AND artifact->>'network_id'=network_id AND (artifact->>'revision')::bigint=revision AND artifact->>'default_action'='deny'),
  CONSTRAINT overlay_policy_revision_secret_ck CHECK (source::text !~* '"(private_key|refresh_token|vault_token|bearer_token|auth_id)"[[:space:]]*:')
);
CREATE UNIQUE INDEX overlay_policy_one_active_per_network_uk ON public.overlay_policy_revisions(network_id) WHERE status='active';
CREATE UNIQUE INDEX overlay_policy_generation_uk ON public.overlay_policy_revisions(network_id,generation) WHERE generation>0;
CREATE INDEX overlay_policy_owner_idx ON public.overlay_policy_revisions(owner_user_uuid,network_id,revision DESC);
CREATE TABLE public.overlay_policy_builds (
 network_id TEXT NOT NULL, generation BIGINT NOT NULL CHECK(generation>0), revision BIGINT NOT NULL,
 artifact JSONB NOT NULL, artifact_sha256 TEXT NOT NULL CHECK(artifact_sha256~'^[a-f0-9]{64}$'), compiler_version TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(network_id,generation),
 FOREIGN KEY(network_id,revision) REFERENCES public.overlay_policy_revisions(network_id,revision) ON DELETE RESTRICT
);
COMMIT;
