-- XConnect Zero formal transport contract: VLESS/XHTTP over TCP 443.
-- Only public transport metadata is stored here; credentials remain in Vault.
BEGIN;

ALTER TABLE public.overlay_networks
  ADD COLUMN IF NOT EXISTS transport_kind TEXT NOT NULL DEFAULT 'vless-xhttp',
  ADD COLUMN IF NOT EXISTS transport_path TEXT NOT NULL DEFAULT '/xconnect',
  ADD COLUMN IF NOT EXISTS transport_mode TEXT NOT NULL DEFAULT 'auto',
  ADD COLUMN IF NOT EXISTS transport_host TEXT NOT NULL DEFAULT '';

UPDATE public.overlay_networks
   SET transport_kind = 'vless-xhttp',
       transport_path = CASE WHEN transport_path = '' THEN '/xconnect' ELSE transport_path END,
       transport_mode = CASE WHEN transport_mode = '' THEN 'auto' ELSE transport_mode END
 WHERE transport_kind IS NULL OR transport_kind = '' OR transport_kind <> 'vless-xhttp'
    OR transport_path IS NULL OR transport_mode IS NULL;

COMMIT;
