BEGIN;
DROP TABLE IF EXISTS public.overlay_device_revoke_receipts;
DROP TABLE IF EXISTS public.overlay_device_sessions;
-- Destructive: this removes historical verifier tombstones and terminal
-- revoke receipts. Stop device traffic and require security approval.
DROP TABLE IF EXISTS public.overlay_device_credentials;
ALTER TABLE public.overlay_devices DROP CONSTRAINT IF EXISTS overlay_devices_user_network_device_uk;
COMMIT;
