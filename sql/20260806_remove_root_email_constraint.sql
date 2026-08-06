BEGIN;
ALTER TABLE public.users DROP CONSTRAINT IF EXISTS users_root_email_ck;
COMMIT;
