-- Independent marketing preferences: never modifies subscriptions or quotas.
CREATE TABLE IF NOT EXISTS public.email_management_settings (
 id TEXT PRIMARY KEY,
 version BIGINT NOT NULL,
 company_name TEXT NOT NULL,
 postal_address TEXT NOT NULL,
 contact_email TEXT NOT NULL,
 console_url TEXT NOT NULL,
 updated_by TEXT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS public.email_preferences (
 user_id TEXT PRIMARY KEY,
 product_updates BOOLEAN NOT NULL,
 tutorials BOOLEAN NOT NULL,
 offers BOOLEAN NOT NULL,
 frequency TEXT NOT NULL,
 version BIGINT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS public.email_preference_tokens (
 hash TEXT PRIMARY KEY,
 user_id TEXT NOT NULL,
 email TEXT NOT NULL,
 purpose TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_email_preference_tokens_user_id ON public.email_preference_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_email_preference_tokens_expires_at ON public.email_preference_tokens(expires_at);
CREATE TABLE IF NOT EXISTS public.email_preference_events (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL,
 source TEXT NOT NULL,
 policy_version TEXT NOT NULL,
 preferences TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_email_preference_events_user_id ON public.email_preference_events(user_id);
