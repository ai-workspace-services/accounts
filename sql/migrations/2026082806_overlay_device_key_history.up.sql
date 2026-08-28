BEGIN;
-- WireGuard authenticates only the key, not device_id. Keep a permanent,
-- non-cascading tombstone so revoked and rotated-away keys can never be
-- reassigned to another device identity.
CREATE TABLE public.overlay_device_key_history(
  network_id TEXT NOT NULL,
  wireguard_public_key TEXT NOT NULL CHECK(btrim(wireguard_public_key)<>''),
  user_uuid UUID NOT NULL,
  device_id TEXT NOT NULL CHECK(btrim(device_id)<>''),
  key_version BIGINT NOT NULL CHECK(key_version>0),
  claimed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(network_id,wireguard_public_key)
);
-- A collision aborts this migration. Never choose a winning identity for a
-- key that was already reused while the temporary partial index was active.
INSERT INTO public.overlay_device_key_history(network_id,wireguard_public_key,user_uuid,device_id,key_version,claimed_at)
SELECT network_id,wireguard_public_key,user_uuid,id,key_version,created_at
FROM public.overlay_devices;
CREATE INDEX overlay_device_key_history_device_idx ON public.overlay_device_key_history(network_id,user_uuid,device_id,key_version);
DROP INDEX IF EXISTS public.overlay_devices_network_active_public_key_uk;
CREATE UNIQUE INDEX overlay_devices_network_public_key_uk ON public.overlay_devices(network_id,wireguard_public_key);
COMMIT;
