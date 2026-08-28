BEGIN;
-- This rollback destroys permanent key-reuse protection. Stop device writes,
-- retain an encrypted copy, and require explicit security approval.
DROP TABLE IF EXISTS public.overlay_device_key_history;
DROP INDEX IF EXISTS public.overlay_devices_network_public_key_uk;
CREATE UNIQUE INDEX overlay_devices_network_active_public_key_uk ON public.overlay_devices(network_id,wireguard_public_key) WHERE status<>'revoked';
COMMIT;
