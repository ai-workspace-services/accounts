-- Additive recovery workflow. Existing MFA and financial records are untouched.
BEGIN;
CREATE TABLE public.mfa_reset_requests (
  id UUID PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  email_snapshot TEXT NOT NULL,
  mfa_fingerprint TEXT NOT NULL,
  code_hash TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending_email', 'pending_review', 'approved', 'rejected', 'expired')),
  reason TEXT NOT NULL DEFAULT '',
  review_reason TEXT NOT NULL DEFAULT '',
  reviewer_uuid UUID REFERENCES public.users(uuid),
  failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
  created_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  authorized_at TIMESTAMPTZ,
  reviewed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX mfa_reset_active_user ON public.mfa_reset_requests(user_uuid)
  WHERE status IN ('pending_email', 'pending_review');
CREATE INDEX mfa_reset_review_queue ON public.mfa_reset_requests(status, created_at);
ALTER TABLE public.mfa_reset_requests ENABLE ROW LEVEL SECURITY;
COMMIT;
