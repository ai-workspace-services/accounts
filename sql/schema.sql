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

CREATE TABLE public.users (
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
  proxy_uuid UUID NOT NULL DEFAULT gen_random_uuid(),
  proxy_uuid_expires_at TIMESTAMPTZ,
  CONSTRAINT users_root_email_ck CHECK (lower(role) <> 'root' OR lower(email) = 'admin@svc.plus')
);

CREATE TABLE public.email_blacklist (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE public.identities (
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

CREATE TABLE public.sessions (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  token TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  version BIGINT NOT NULL DEFAULT 0,
  origin_node TEXT NOT NULL DEFAULT 'local'
);

CREATE TABLE public.agents (
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

CREATE TABLE public.admin_settings (
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

CREATE TABLE public.rbac_roles (
  role_key TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 100,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE public.rbac_permissions (
  permission_key TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE public.rbac_role_permissions (
  role_key TEXT NOT NULL REFERENCES public.rbac_roles(role_key) ON DELETE CASCADE,
  permission_key TEXT NOT NULL REFERENCES public.rbac_permissions(permission_key) ON DELETE CASCADE,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (role_key, permission_key)
);

CREATE TABLE public.subscriptions (
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

CREATE TABLE public.nodes (
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

-- =========================================
-- Indexes
-- =========================================
CREATE UNIQUE INDEX users_username_lower_uk ON public.users (lower(username));
CREATE UNIQUE INDEX users_email_lower_uk ON public.users (lower(email)) WHERE email IS NOT NULL;
CREATE UNIQUE INDEX users_single_root_role_uk ON public.users ((lower(role))) WHERE lower(role) = 'root';
CREATE INDEX idx_identities_user_uuid ON public.identities (user_uuid);
CREATE INDEX idx_sessions_user_uuid ON public.sessions (user_uuid);
CREATE UNIQUE INDEX sessions_token_uk ON public.sessions (token);
CREATE INDEX idx_admin_settings_version ON public.admin_settings (version);
CREATE INDEX idx_subscriptions_user_uuid ON public.subscriptions (user_uuid);
CREATE INDEX idx_subscriptions_status ON public.subscriptions (status);
CREATE INDEX idx_nodes_available ON public.nodes (available);

-- =========================================
-- Triggers
-- =========================================

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
