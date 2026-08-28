-- Accounts-only authorization reads the most recent reviewed static import for
-- one network. Keep this control-plane lookup bounded as receipt history grows.
CREATE INDEX IF NOT EXISTS overlay_static_import_receipts_network_created_idx
  ON public.overlay_static_import_receipts (network_id, created_at DESC, import_id DESC);
