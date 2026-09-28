-- Run only against a new, disposable PostgreSQL database.
\set ON_ERROR_STOP on

DO $$
BEGIN
  IF to_regclass('public.users') IS NOT NULL
     OR to_regclass('public.subscriptions') IS NOT NULL
     OR to_regclass('public.billing_ledger') IS NOT NULL THEN
    RAISE EXCEPTION 'test requires a fresh database without account/billing tables';
  END IF;
END;
$$;

CREATE TABLE public.users (uuid UUID PRIMARY KEY, email TEXT NOT NULL);
CREATE TABLE public.subscriptions (
  uuid UUID PRIMARY KEY,
  user_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  status TEXT NOT NULL
);
CREATE TABLE public.billing_ledger (
  id UUID PRIMARY KEY,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE CASCADE,
  entry_type TEXT NOT NULL,
  rated_bytes BIGINT NOT NULL
);
INSERT INTO public.users VALUES ('00000000-0000-4000-8000-000000000001', 'sentinel@example.invalid');
INSERT INTO public.subscriptions VALUES ('00000000-0000-4000-8000-000000000002', '00000000-0000-4000-8000-000000000001', 'active');
INSERT INTO public.billing_ledger VALUES ('00000000-0000-4000-8000-000000000003', '00000000-0000-4000-8000-000000000001', 'usage', 12345);

CREATE TEMP TABLE finance_subscription_snapshot AS SELECT uuid, to_jsonb(subscriptions) AS row_data FROM public.subscriptions;
CREATE TEMP TABLE finance_usage_snapshot AS SELECT id, to_jsonb(billing_ledger) AS row_data FROM public.billing_ledger;

\ir ../2026092801_local_finance_ledger.up.sql
\ir ../2026092801_local_finance_ledger.up.sql

INSERT INTO public.finance_invoices (
  id, idempotency_key, account_uuid, subscription_uuid, amount_minor, currency, description
) VALUES (
  '00000000-0000-4000-8000-000000000101', 'invoice:sentinel',
  '00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002', 1000, 'USD', 'fixture'
);
INSERT INTO public.finance_payments (
  id, idempotency_key, invoice_id, account_uuid, amount_minor, currency
) VALUES (
  '00000000-0000-4000-8000-000000000201', 'payment:sentinel',
  '00000000-0000-4000-8000-000000000101', '00000000-0000-4000-8000-000000000001', 1000, 'USD'
);
INSERT INTO public.finance_refunds (
  id, idempotency_key, payment_id, amount_minor, currency
) VALUES (
  '00000000-0000-4000-8000-000000000301', 'refund:sentinel',
  '00000000-0000-4000-8000-000000000201', 250, 'USD'
);
INSERT INTO public.finance_operations (id, idempotency_key, operation_type, target_type, target_id, status, request)
VALUES (
  '00000000-0000-4000-8000-000000000401', 'refund-op:sentinel', 'refund', 'payment',
  '00000000-0000-4000-8000-000000000201', 'pending', '{"amount_minor":100,"currency":"USD"}'::jsonb
);
INSERT INTO public.finance_operation_events (operation_id, attempt, event_type, status)
VALUES ('00000000-0000-4000-8000-000000000401', 1, 'attempt_started', 'in_progress');
UPDATE public.finance_operations SET status = 'succeeded', updated_at = now()
WHERE id = '00000000-0000-4000-8000-000000000401';
INSERT INTO public.finance_operation_events (operation_id, attempt, event_type, status)
VALUES ('00000000-0000-4000-8000-000000000401', 1, 'attempt_succeeded', 'succeeded');

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM finance_subscription_snapshot old JOIN public.subscriptions current USING (uuid) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM finance_subscription_snapshot) <> (SELECT count(*) FROM public.subscriptions) THEN
    RAISE EXCEPTION 'migration changed or removed existing subscriptions';
  END IF;
  IF EXISTS (SELECT 1 FROM finance_usage_snapshot old JOIN public.billing_ledger current USING (id) WHERE old.row_data <> to_jsonb(current))
     OR (SELECT count(*) FROM finance_usage_snapshot) <> (SELECT count(*) FROM public.billing_ledger) THEN
    RAISE EXCEPTION 'migration changed or removed usage billing_ledger entries';
  END IF;
  IF (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN (
      'finance_invoices', 'finance_payments', 'finance_refunds', 'finance_operations', 'finance_operation_events'
    )) <> 5 THEN
    RAISE EXCEPTION 'finance migration objects were not installed exactly once';
  END IF;
  IF (SELECT status FROM public.finance_operations WHERE idempotency_key = 'refund-op:sentinel') <> 'succeeded'
     OR (SELECT count(*) FROM public.finance_operation_events WHERE operation_id = '00000000-0000-4000-8000-000000000401') <> 2 THEN
    RAISE EXCEPTION 'mutable operation status did not retain its append-only event history';
  END IF;
END;
$$;

DO $$
BEGIN
  BEGIN
    UPDATE public.finance_invoices SET description = 'tampered' WHERE id = '00000000-0000-4000-8000-000000000101';
    RAISE EXCEPTION 'append-only invoice update unexpectedly succeeded';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM = 'append-only invoice update unexpectedly succeeded' THEN RAISE; END IF;
  END;
  BEGIN
    DELETE FROM public.finance_refunds WHERE id = '00000000-0000-4000-8000-000000000301';
    RAISE EXCEPTION 'append-only refund delete unexpectedly succeeded';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM = 'append-only refund delete unexpectedly succeeded' THEN RAISE; END IF;
  END;
  BEGIN
    EXECUTE 'TRUNCATE public.finance_operation_events';
    RAISE EXCEPTION 'append-only operation event truncate unexpectedly succeeded';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM = 'append-only operation event truncate unexpectedly succeeded' THEN RAISE; END IF;
  END;
  BEGIN
    UPDATE public.finance_operation_events SET error = 'tampered'
    WHERE operation_id = '00000000-0000-4000-8000-000000000401';
    RAISE EXCEPTION 'append-only operation event update unexpectedly succeeded';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM = 'append-only operation event update unexpectedly succeeded' THEN RAISE; END IF;
  END;
  BEGIN
    DELETE FROM public.users WHERE uuid = '00000000-0000-4000-8000-000000000001';
    RAISE EXCEPTION 'finance foreign key allowed user cascade deletion';
  EXCEPTION WHEN foreign_key_violation THEN
    NULL;
  END;
END;
$$;
