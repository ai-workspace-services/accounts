-- Apply the same finance-table access guard to databases that may already
-- have run the original 2026092801 migration. No retained rows are changed.
BEGIN;

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

COMMIT;
