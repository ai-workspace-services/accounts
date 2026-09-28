-- P0 local financial facts and durable provider-operation state.
-- This migration is additive: legacy subscriptions and usage billing_ledger
-- rows are not transformed or used as payment/refund records.

BEGIN;

CREATE TABLE IF NOT EXISTS public.finance_invoices (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key TEXT NOT NULL UNIQUE,
  account_uuid UUID NOT NULL REFERENCES public.users(uuid) ON DELETE RESTRICT,
  subscription_uuid UUID NULL REFERENCES public.subscriptions(uuid) ON DELETE RESTRICT,
  provider TEXT NOT NULL DEFAULT 'local',
  provider_invoice_id TEXT NULL,
  amount_minor BIGINT NOT NULL CHECK (amount_minor >= 0),
  currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  description TEXT NOT NULL DEFAULT '',
  issued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  due_at TIMESTAMPTZ NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT finance_invoices_payment_contract_uk UNIQUE (id, account_uuid, amount_minor, currency),
  CONSTRAINT finance_invoices_provider_external_uk UNIQUE (provider, provider_invoice_id)
);

CREATE INDEX IF NOT EXISTS finance_invoices_account_created_idx
  ON public.finance_invoices (account_uuid, created_at DESC);
CREATE INDEX IF NOT EXISTS finance_invoices_subscription_created_idx
  ON public.finance_invoices (subscription_uuid, created_at DESC)
  WHERE subscription_uuid IS NOT NULL;

CREATE TABLE IF NOT EXISTS public.finance_payments (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key TEXT NOT NULL UNIQUE,
  invoice_id UUID NOT NULL,
  account_uuid UUID NOT NULL,
  provider TEXT NOT NULL DEFAULT 'local',
  provider_payment_id TEXT NULL,
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
  currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  paid_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT finance_payments_invoice_contract_fk FOREIGN KEY (invoice_id, account_uuid, amount_minor, currency)
    REFERENCES public.finance_invoices(id, account_uuid, amount_minor, currency) ON DELETE RESTRICT,
  CONSTRAINT finance_payments_id_currency_uk UNIQUE (id, currency),
  CONSTRAINT finance_payments_provider_external_uk UNIQUE (provider, provider_payment_id)
);

CREATE INDEX IF NOT EXISTS finance_payments_account_created_idx
  ON public.finance_payments (account_uuid, created_at DESC);
CREATE INDEX IF NOT EXISTS finance_payments_invoice_created_idx
  ON public.finance_payments (invoice_id, created_at DESC);

CREATE TABLE IF NOT EXISTS public.finance_refunds (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key TEXT NOT NULL UNIQUE,
  payment_id UUID NOT NULL REFERENCES public.finance_payments(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL DEFAULT 'local',
  provider_refund_id TEXT NULL,
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
  currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  reason TEXT NOT NULL DEFAULT '',
  refunded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT finance_refunds_payment_currency_fk FOREIGN KEY (payment_id, currency)
    REFERENCES public.finance_payments(id, currency) ON DELETE RESTRICT,
  CONSTRAINT finance_refunds_provider_external_uk UNIQUE (provider, provider_refund_id)
);

CREATE INDEX IF NOT EXISTS finance_refunds_payment_created_idx
  ON public.finance_refunds (payment_id, created_at DESC);

-- Mutable operation projection; its event stream retains every attempt and result.
CREATE TABLE IF NOT EXISTS public.finance_operations (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key TEXT NOT NULL UNIQUE,
  operation_type TEXT NOT NULL,
  target_type TEXT NOT NULL DEFAULT '',
  target_id TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT 'local',
  provider_operation_id TEXT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'in_progress', 'succeeded', 'failed', 'reconcile_needed')),
  attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
  next_attempt_at TIMESTAMPTZ NULL,
  last_error TEXT NOT NULL DEFAULT '',
  request JSONB NOT NULL DEFAULT '{}'::jsonb,
  response JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT finance_operations_provider_external_uk UNIQUE (provider, operation_type, provider_operation_id)
);

CREATE INDEX IF NOT EXISTS finance_operations_reconcile_idx
  ON public.finance_operations (status, next_attempt_at, updated_at)
  WHERE status IN ('pending', 'failed', 'reconcile_needed');

CREATE TABLE IF NOT EXISTS public.finance_operation_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  operation_id UUID NOT NULL REFERENCES public.finance_operations(id) ON DELETE RESTRICT,
  attempt INTEGER NOT NULL CHECK (attempt > 0),
  event_type TEXT NOT NULL,
  status TEXT NOT NULL,
  provider_operation_id TEXT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  error TEXT NOT NULL DEFAULT '',
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS finance_operation_events_operation_idx
  ON public.finance_operation_events (operation_id, id);

-- Supabase exposes public-schema tables through its API roles by default.
-- These financial facts and provider payloads are server-side only. RLS has
-- no client policies, and grants to the client roles (including PUBLIC) are
-- removed in the same transaction that creates the tables.
ALTER TABLE public.finance_invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.finance_payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.finance_refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.finance_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.finance_operation_events ENABLE ROW LEVEL SECURITY;

REVOKE ALL PRIVILEGES ON TABLE
  public.finance_invoices,
  public.finance_payments,
  public.finance_refunds,
  public.finance_operations,
  public.finance_operation_events
FROM PUBLIC;

DO $$
DECLARE
  client_role TEXT;
BEGIN
  FOREACH client_role IN ARRAY ARRAY['anon', 'authenticated'] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = client_role) THEN
      EXECUTE format(
        'REVOKE ALL PRIVILEGES ON TABLE public.finance_invoices, public.finance_payments, public.finance_refunds, public.finance_operations, public.finance_operation_events FROM %I',
        client_role
      );
    END IF;
  END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION public.reject_finance_fact_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only; % is not permitted', TG_TABLE_NAME, TG_OP;
  RETURN OLD;
END;
$$;

CREATE OR REPLACE FUNCTION public.validate_finance_invoice_subscription()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.subscription_uuid IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM public.subscriptions s
    WHERE s.uuid = NEW.subscription_uuid AND s.user_uuid = NEW.account_uuid
  ) THEN
    RAISE EXCEPTION 'subscription % does not belong to account %', NEW.subscription_uuid, NEW.account_uuid;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS finance_invoice_subscription_owner ON public.finance_invoices;
CREATE TRIGGER finance_invoice_subscription_owner
  BEFORE INSERT ON public.finance_invoices
  FOR EACH ROW EXECUTE FUNCTION public.validate_finance_invoice_subscription();

DROP TRIGGER IF EXISTS finance_invoices_append_only ON public.finance_invoices;
CREATE TRIGGER finance_invoices_append_only
  BEFORE UPDATE OR DELETE ON public.finance_invoices
  FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_invoices_no_truncate ON public.finance_invoices;
CREATE TRIGGER finance_invoices_no_truncate BEFORE TRUNCATE ON public.finance_invoices
  FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_payments_append_only ON public.finance_payments;
CREATE TRIGGER finance_payments_append_only
  BEFORE UPDATE OR DELETE ON public.finance_payments
  FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_payments_no_truncate ON public.finance_payments;
CREATE TRIGGER finance_payments_no_truncate BEFORE TRUNCATE ON public.finance_payments
  FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_refunds_append_only ON public.finance_refunds;
CREATE TRIGGER finance_refunds_append_only
  BEFORE UPDATE OR DELETE ON public.finance_refunds
  FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_refunds_no_truncate ON public.finance_refunds;
CREATE TRIGGER finance_refunds_no_truncate BEFORE TRUNCATE ON public.finance_refunds
  FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_operation_events_append_only ON public.finance_operation_events;
CREATE TRIGGER finance_operation_events_append_only
  BEFORE UPDATE OR DELETE ON public.finance_operation_events
  FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_operation_events_no_truncate ON public.finance_operation_events;
CREATE TRIGGER finance_operation_events_no_truncate BEFORE TRUNCATE ON public.finance_operation_events
  FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_operations_no_delete ON public.finance_operations;
CREATE TRIGGER finance_operations_no_delete
  BEFORE DELETE ON public.finance_operations
  FOR EACH ROW EXECUTE FUNCTION public.reject_finance_fact_mutation();
DROP TRIGGER IF EXISTS finance_operations_no_truncate ON public.finance_operations;
CREATE TRIGGER finance_operations_no_truncate BEFORE TRUNCATE ON public.finance_operations
  FOR EACH STATEMENT EXECUTE FUNCTION public.reject_finance_fact_mutation();

COMMIT;
