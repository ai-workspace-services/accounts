-- schema.sql
-- Base business schema for the account service.
-- Works with both one-way async sync (pgsync) and pglogical multi-master.
-- PostgreSQL 16 + gen_random_uuid()
-- =========================================

-- Ensure the public schema exists without dropping other extensions.
CREATE SCHEMA IF NOT EXISTS public AUTHORIZATION CURRENT_USER;

-- Clean up existing tables so the script is idempotent without requiring
-- superuser privileges that would be needed to drop the entire schema.
DROP TABLE IF EXISTS public.sessions CASCADE;
DROP TABLE IF EXISTS public.identities CASCADE;
DROP TABLE IF EXISTS public.overlay_config_acks CASCADE;
DROP TABLE IF EXISTS public.overlay_signed_config_acks CASCADE;
DROP TABLE IF EXISTS public.overlay_signed_configs CASCADE;
DROP TABLE IF EXISTS public.overlay_enrollment_sessions CASCADE;
DROP TABLE IF EXISTS public.overlay_join_tokens CASCADE;
DROP TABLE IF EXISTS public.overlay_devices CASCADE;
DROP TABLE IF EXISTS public.overlay_nodes CASCADE;
DROP TABLE IF EXISTS public.users CASCADE;
DROP TABLE IF EXISTS public.admin_settings CASCADE;
DROP TABLE IF EXISTS public.subscriptions CASCADE;
DROP TABLE IF EXISTS public.rbac_role_permissions CASCADE;
DROP TABLE IF EXISTS public.rbac_permissions CASCADE;
DROP TABLE IF EXISTS public.rbac_roles CASCADE;

-- =========================================
-- Extensions
-- =========================================
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- pglogical specific defaults are now applied by schema_pglogical_patch.sql.

-- =========================================
-- Functions
-- =========================================

-- 更新时间戳
CREATE OR REPLACE FUNCTION public.set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;

-- 邮箱验证标志维护
CREATE OR REPLACE FUNCTION public.maintain_email_verified() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.email_verified := (NEW.email_verified_at IS NOT NULL);
  RETURN NEW;
END;
$$;

-- 双向复制版本号自增触发器
CREATE OR REPLACE FUNCTION public.bump_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    NEW.version := COALESCE(OLD.version, 0) + 1;
  END IF;
  RETURN NEW;
END;
$$;

-- Tables
-- =========================================

CREATE TABLE IF NOT EXISTS public.users (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  username TEXT NOT NULL,
  password TEXT NOT NULL,
  email TEXT,
  role TEXT NOT NULL DEFAULT 'user',
  level INTEGER NOT NULL DEFAULT 20,
  groups JSONB NOT NULL DEFAULT '[]'::jsonb,
  permissions JSONB NOT NULL DEFAULT '[]'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 0, -- 🔢 行版本号
  origin_node TEXT NOT NULL DEFAULT 'local', -- 🌍 来源节点，可在不同区域通过 ALTER TABLE 或 pglogical patch 覆盖
  mfa_totp_secret TEXT,
  mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  mfa_secret_issued_at TIMESTAMPTZ,
  mfa_confirmed_at TIMESTAMPTZ,
  email_verified_at TIMESTAMPTZ,
  email_verified BOOLEAN GENERATED ALWAYS AS ((email_verified_at IS NOT NULL)) STORED,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  proxy_uuid UUID NOT NULL,
  proxy_uuid_expires_at TIMESTAMPTZ
);


CREATE TABLE IF NOT EXISTS public.email_blacklist (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.identities (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  provider TEXT NOT NULL,
  external_id TEXT NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 0,
  origin_node TEXT NOT NULL DEFAULT 'local',
  CONSTRAINT identities_provider_external_id_uk UNIQUE (provider, external_id)
);

CREATE TABLE IF NOT EXISTS public.sessions (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  token TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 0,
  origin_node TEXT NOT NULL DEFAULT 'local'
);

CREATE TABLE IF NOT EXISTS public.agents (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  groups JSONB NOT NULL DEFAULT '[]'::jsonb,
  healthy BOOLEAN NOT NULL DEFAULT FALSE,
  last_heartbeat TIMESTAMPTZ,
  clients_count INTEGER NOT NULL DEFAULT 0,
  sync_revision TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.overlay_devices (
  id TEXT NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  network_id TEXT NOT NULL DEFAULT 'xworkmate-private',
  name TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT '',
  hostname TEXT NOT NULL DEFAULT '',
  wireguard_public_key TEXT NOT NULL,
  wireguard_address TEXT NOT NULL,
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, id)
);

CREATE TABLE IF NOT EXISTS public.overlay_nodes (
  id TEXT PRIMARY KEY,
  network_id TEXT NOT NULL DEFAULT 'xworkmate-private',
  name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'gateway',
  region TEXT NOT NULL DEFAULT '',
  wireguard_public_key TEXT NOT NULL,
  wireguard_address TEXT NOT NULL,
  endpoint_host TEXT NOT NULL,
  endpoint_port INTEGER NOT NULL DEFAULT 2443,
  transport_type TEXT NOT NULL DEFAULT 'vless-tls',
  transport_security TEXT NOT NULL DEFAULT 'tls',
  transport_path TEXT NOT NULL DEFAULT '',
  transport_mode TEXT NOT NULL DEFAULT '',
  transport_uuid TEXT NOT NULL DEFAULT '',
  healthy BOOLEAN NOT NULL DEFAULT FALSE,
  last_heartbeat TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.overlay_config_acks (
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  device_id TEXT NOT NULL,
  network_id TEXT NOT NULL DEFAULT 'xworkmate-private',
  revision TEXT NOT NULL,
  digest TEXT NOT NULL DEFAULT '',
  applied_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, device_id),
  FOREIGN KEY (user_uuid, device_id) REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS public.overlay_signed_configs (
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  device_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  config_id TEXT NOT NULL,
  network_id TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  signing_key_id TEXT NOT NULL,
  signed_payload JSONB NOT NULL,
  issued_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, device_id, generation),
  CONSTRAINT overlay_signed_configs_config_uk UNIQUE (user_uuid, device_id, config_id),
  CONSTRAINT overlay_signed_configs_ack_target_uk UNIQUE (user_uuid, device_id, generation, config_id),
  CONSTRAINT overlay_signed_configs_device_fk FOREIGN KEY (user_uuid, device_id)
    REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE,
  CONSTRAINT overlay_signed_configs_xray_ck CHECK (signed_payload->>'proxy_core' = 'xray'),
  CONSTRAINT overlay_signed_configs_envelope_ck CHECK (
    signed_payload->>'config_id' = config_id
    AND signed_payload->>'network_id' = network_id
    AND signed_payload->>'device_id' = device_id
    AND (signed_payload->>'generation')::bigint = generation
    AND signed_payload->'signature'->>'key_id' = signing_key_id
  ),
  CONSTRAINT overlay_signed_configs_lifetime_ck CHECK (expires_at > issued_at),
  CONSTRAINT overlay_signed_configs_source_ck CHECK (btrim(source_revision) <> ''),
  CONSTRAINT overlay_signed_configs_no_secret_keys_ck CHECK (
    signed_payload::text !~* '"(private_key|refresh_token|vault_token)"[[:space:]]*:'
  )
);

CREATE INDEX IF NOT EXISTS overlay_signed_configs_latest_idx
  ON public.overlay_signed_configs (user_uuid, device_id, generation DESC);

CREATE TABLE IF NOT EXISTS public.overlay_signed_config_acks (
  user_uuid UUID NOT NULL,
  device_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  config_id TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_uuid, device_id, generation),
  CONSTRAINT overlay_signed_config_acks_config_fk
    FOREIGN KEY (user_uuid, device_id, generation, config_id)
    REFERENCES public.overlay_signed_configs(user_uuid, device_id, generation, config_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS public.overlay_join_tokens (
  id TEXT PRIMARY KEY,
  token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  network_id TEXT NOT NULL CHECK (btrim(network_id) <> ''),
  device_id TEXT,
  platform TEXT,
  remaining_uses INTEGER NOT NULL CHECK (remaining_uses IN (0, 1)),
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  last_exchanged_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT overlay_join_tokens_device_ck CHECK (device_id IS NULL OR btrim(device_id) <> ''),
  CONSTRAINT overlay_join_tokens_platform_ck CHECK (platform IS NULL OR platform IN ('darwin', 'windows', 'linux', 'ios', 'android')),
  CONSTRAINT overlay_join_tokens_expiry_ck CHECK (expires_at > created_at),
  CONSTRAINT overlay_join_tokens_scope_uk UNIQUE (id, user_uuid, network_id)
);

CREATE INDEX IF NOT EXISTS overlay_join_tokens_owner_idx
  ON public.overlay_join_tokens (user_uuid, created_at DESC);
CREATE INDEX IF NOT EXISTS overlay_join_tokens_expiry_idx
  ON public.overlay_join_tokens (expires_at) WHERE revoked_at IS NULL AND remaining_uses > 0;

CREATE TABLE IF NOT EXISTS public.overlay_enrollment_sessions (
  id TEXT PRIMARY KEY,
  token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  join_token_id TEXT NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  network_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  platform TEXT NOT NULL CHECK (platform IN ('darwin', 'windows', 'linux', 'ios', 'android')),
  wireguard_public_key TEXT NOT NULL CHECK (btrim(wireguard_public_key) <> ''),
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  CONSTRAINT overlay_enrollment_sessions_join_device_uk UNIQUE (join_token_id, device_id),
  CONSTRAINT overlay_enrollment_sessions_join_scope_fk FOREIGN KEY (join_token_id, user_uuid, network_id)
    REFERENCES public.overlay_join_tokens(id, user_uuid, network_id) ON DELETE CASCADE,
  CONSTRAINT overlay_enrollment_sessions_device_fk FOREIGN KEY (user_uuid, device_id)
    REFERENCES public.overlay_devices(user_uuid, id) ON DELETE CASCADE,
  CONSTRAINT overlay_enrollment_sessions_expiry_ck CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS overlay_enrollment_sessions_expiry_idx
  ON public.overlay_enrollment_sessions (expires_at);

CREATE INDEX IF NOT EXISTS idx_overlay_devices_network ON public.overlay_devices(network_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_overlay_devices_network_address
  ON public.overlay_devices(network_id, wireguard_address);
CREATE INDEX IF NOT EXISTS idx_overlay_nodes_network ON public.overlay_nodes(network_id);

-- The account service also creates these tables during startup via GORM.  Keep
-- the baseline safe when startup races this initializer: the baseline owns the
-- schema definition, but must not fail just because GORM won the CREATE race.
CREATE TABLE IF NOT EXISTS public.admin_settings (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  module_key TEXT NOT NULL,
  role TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT FALSE,
  version BIGINT NOT NULL DEFAULT 1,
  origin_node TEXT NOT NULL DEFAULT 'local',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT admin_settings_module_role_uk UNIQUE (module_key, role)
);

CREATE TABLE IF NOT EXISTS public.rbac_roles (
  role_key TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 100,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.rbac_permissions (
  permission_key TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.rbac_role_permissions (
  role_key TEXT NOT NULL REFERENCES public.rbac_roles(role_key) ON DELETE CASCADE,
  permission_key TEXT NOT NULL REFERENCES public.rbac_permissions(permission_key) ON DELETE CASCADE,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (role_key, permission_key)
);

CREATE TABLE IF NOT EXISTS public.subscriptions (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  payment_method TEXT NOT NULL DEFAULT 'paypal',
  kind TEXT NOT NULL DEFAULT 'subscription',
  plan_id TEXT,
  external_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  payment_qr TEXT,
  meta JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  cancelled_at TIMESTAMPTZ,
  CONSTRAINT subscriptions_user_external_uk UNIQUE (user_uuid, external_id)
);

-- Billing plan catalog (billing P1): maps Stripe prices to entitlements.
-- Data-driven: price/quota adjustments are catalog edits, never deploys.
CREATE TABLE IF NOT EXISTS public.billing_plans (
  plan_id TEXT PRIMARY KEY,
  stripe_price_id TEXT UNIQUE,
  display_name TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'subscription',
  included_quota_bytes BIGINT NOT NULL DEFAULT 0,
  package_name TEXT NOT NULL DEFAULT 'default',
  price_amount BIGINT NOT NULL DEFAULT 0,
  price_currency TEXT NOT NULL DEFAULT '',
  price_unit TEXT NOT NULL DEFAULT '',
  price_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb,
  features JSONB NOT NULL DEFAULT '{}'::jsonb,
  trial_days INTEGER NOT NULL DEFAULT 0,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stripe webhook audit/dedup trail (billing P1).
CREATE TABLE IF NOT EXISTS public.stripe_webhook_events (
  event_id TEXT PRIMARY KEY,
  event_type TEXT NOT NULL DEFAULT '',
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'received',
  last_error TEXT NOT NULL DEFAULT '',
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS stripe_webhook_events_received_at_idx ON public.stripe_webhook_events (received_at DESC);

CREATE TABLE IF NOT EXISTS public.nodes (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL,
  location TEXT NOT NULL,
  address TEXT NOT NULL,
  port INTEGER NOT NULL DEFAULT 443,
  server_name TEXT,
  protocols JSONB NOT NULL DEFAULT '[]'::jsonb,
  available BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 0,
  origin_node TEXT NOT NULL DEFAULT 'local'
);

-- Audit log trail (prevents full-table scans & limits migration log bloat)
CREATE TABLE IF NOT EXISTS public.audit_logs (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  action TEXT NOT NULL DEFAULT '',
  actor_uuid UUID,
  details JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Multi-tenant bridge credentials. users.uuid remains the permanent account
-- key; credential_uuid is the rotatable external/Xray identity.
CREATE TABLE IF NOT EXISTS public.tenants (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  edition TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS public.tenant_domains (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  domain TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  is_primary BOOLEAN NOT NULL DEFAULT FALSE,
  status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS public.tenant_memberships (
  tenant_id TEXT NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);
CREATE TABLE IF NOT EXISTS public.bridge_credentials (
  credential_uuid UUID PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,
  token_hash TEXT,
  token_prefix TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active',
  source TEXT NOT NULL DEFAULT 'generated',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);

-- =========================================
-- Indexes
-- =========================================
CREATE UNIQUE INDEX IF NOT EXISTS users_username_lower_uk ON public.users (lower(username));
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_uk ON public.users (lower(email)) WHERE email IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_identities_user_uuid ON public.identities (user_uuid);
CREATE INDEX IF NOT EXISTS idx_sessions_user_uuid ON public.sessions (user_uuid);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_token_uk ON public.sessions (token);
CREATE INDEX IF NOT EXISTS idx_admin_settings_version ON public.admin_settings (version);
CREATE INDEX IF NOT EXISTS idx_subscriptions_user_uuid ON public.subscriptions (user_uuid);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON public.subscriptions (status);
CREATE INDEX IF NOT EXISTS idx_nodes_available ON public.nodes (available);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON public.audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action_created_at ON public.audit_logs (action, created_at DESC);
CREATE INDEX IF NOT EXISTS bridge_credentials_user_tenant_idx ON public.bridge_credentials (user_uuid, tenant_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS bridge_credentials_active_user_tenant_uk ON public.bridge_credentials (user_uuid, tenant_id) WHERE status = 'active';

-- =========================================
-- Triggers
-- =========================================

DROP TRIGGER IF EXISTS trg_users_set_updated_at ON public.users;
DROP TRIGGER IF EXISTS trg_users_maintain_email_verified ON public.users;
DROP TRIGGER IF EXISTS trg_users_bump_version ON public.users;
DROP TRIGGER IF EXISTS trg_identities_set_updated_at ON public.identities;
DROP TRIGGER IF EXISTS trg_identities_bump_version ON public.identities;
DROP TRIGGER IF EXISTS trg_sessions_set_updated_at ON public.sessions;
DROP TRIGGER IF EXISTS trg_sessions_bump_version ON public.sessions;
DROP TRIGGER IF EXISTS trg_agents_set_updated_at ON public.agents;
DROP TRIGGER IF EXISTS trg_admin_settings_set_updated_at ON public.admin_settings;
DROP TRIGGER IF EXISTS trg_admin_settings_bump_version ON public.admin_settings;
DROP TRIGGER IF EXISTS trg_rbac_roles_set_updated_at ON public.rbac_roles;
DROP TRIGGER IF EXISTS trg_rbac_permissions_set_updated_at ON public.rbac_permissions;
DROP TRIGGER IF EXISTS trg_rbac_role_permissions_set_updated_at ON public.rbac_role_permissions;
DROP TRIGGER IF EXISTS trg_subscriptions_set_updated_at ON public.subscriptions;
DROP TRIGGER IF EXISTS trg_nodes_set_updated_at ON public.nodes;
DROP TRIGGER IF EXISTS trg_nodes_bump_version ON public.nodes;

-- users
CREATE TRIGGER trg_users_set_updated_at
  BEFORE UPDATE ON public.users
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER trg_users_maintain_email_verified
  BEFORE INSERT OR UPDATE ON public.users
  FOR EACH ROW EXECUTE FUNCTION public.maintain_email_verified();

CREATE TRIGGER trg_users_bump_version
  BEFORE UPDATE ON public.users
  FOR EACH ROW EXECUTE FUNCTION public.bump_version();

-- identities
CREATE TRIGGER trg_identities_set_updated_at
  BEFORE UPDATE ON public.identities
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER trg_identities_bump_version
  BEFORE UPDATE ON public.identities
  FOR EACH ROW EXECUTE FUNCTION public.bump_version();

-- sessions
CREATE TRIGGER trg_sessions_set_updated_at
  BEFORE UPDATE ON public.sessions
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER trg_sessions_bump_version
  BEFORE UPDATE ON public.sessions
  FOR EACH ROW EXECUTE FUNCTION public.bump_version();

-- agents
CREATE TRIGGER trg_agents_set_updated_at
  BEFORE UPDATE ON public.agents
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

-- admin_settings
CREATE TRIGGER trg_admin_settings_set_updated_at
  BEFORE UPDATE ON public.admin_settings
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER trg_admin_settings_bump_version
  BEFORE UPDATE ON public.admin_settings
  FOR EACH ROW EXECUTE FUNCTION public.bump_version();

-- rbac_roles
CREATE TRIGGER trg_rbac_roles_set_updated_at
  BEFORE UPDATE ON public.rbac_roles
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

-- rbac_permissions
CREATE TRIGGER trg_rbac_permissions_set_updated_at
  BEFORE UPDATE ON public.rbac_permissions
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

-- rbac_role_permissions
CREATE TRIGGER trg_rbac_role_permissions_set_updated_at
  BEFORE UPDATE ON public.rbac_role_permissions
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

-- subscriptions
CREATE TRIGGER trg_subscriptions_set_updated_at
  BEFORE UPDATE ON public.subscriptions
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

-- nodes
CREATE TRIGGER trg_nodes_set_updated_at
  BEFORE UPDATE ON public.nodes
  FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER trg_nodes_bump_version
  BEFORE UPDATE ON public.nodes
  FOR EACH ROW EXECUTE FUNCTION public.bump_version();

-- =========================================
-- Seed RBAC
-- =========================================
INSERT INTO public.rbac_roles (role_key, description, priority) VALUES
  ('root', 'single root account', 0),
  ('operator', 'operation role with configurable permissions', 10),
  ('user', 'standard subscription user', 20),
  ('readonly', 'read-only experience account', 30)
ON CONFLICT (role_key) DO NOTHING;

INSERT INTO public.rbac_permissions (permission_key, description) VALUES
  ('admin.settings.read', 'read admin matrix settings'),
  ('admin.settings.write', 'update admin matrix settings'),
  ('admin.users.metrics.read', 'read user metrics'),
  ('admin.users.list.read', 'read user list'),
  ('admin.agents.status.read', 'read agent status'),
  ('admin.users.pause.write', 'pause users'),
  ('admin.users.resume.write', 'resume users'),
  ('admin.users.delete.write', 'delete users'),
  ('admin.users.renew_uuid.write', 'renew user proxy uuid'),
  ('admin.users.role.write', 'update/reset user role'),
  ('admin.blacklist.read', 'read blacklist'),
  ('admin.blacklist.write', 'update blacklist')
ON CONFLICT (permission_key) DO NOTHING;

INSERT INTO public.rbac_role_permissions (role_key, permission_key, enabled)
SELECT 'operator', permission_key, true
FROM public.rbac_permissions
ON CONFLICT (role_key, permission_key) DO NOTHING;
