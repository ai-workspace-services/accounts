-- Per-network XConnect Zero account/member bindings for gateway access and enrollment.
BEGIN;

CREATE TABLE IF NOT EXISTS public.overlay_network_members (
  id TEXT NOT NULL,
  network_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  email TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_overlay_network_member
  ON public.overlay_network_members (network_id, user_id);

COMMIT;
