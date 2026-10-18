# Durable subscription refunds

The self-service refund endpoint persists a local finance operation before the
provider request. The operation key uses the local subscription UUID; Stripe
refund references remain external attributes, not business primary keys.

## Safety contract

- Existing MFA, first-subscription, seven-day and usage eligibility gates remain.
- Validate account, invoice, subscription, payment, currency and amount together.
- Reserve refund capacity while an operation is pending or needs reconciliation.
- Persist the external-request boundary before calling Stripe. After an ambiguous
  result, query the provider rather than blindly submitting another refund.
- Associate a webhook with an operation only after validating immutable local
  facts. Write the refund fact, provider reference and audit event atomically.
- A provider-successful refund does not complete the intent until cancellation
  and local entitlement handling also succeed. Preserve other paid subscriptions.
- Retry the same endpoint to reconcile an interrupted attempt. An expired attempt
  may be reclaimed after two minutes; attempt fencing prevents stale completion.

No new schema, background scheduler, user deletion, ledger deletion or production
deployment is introduced. Recovery currently requires an endpoint retry, not an
autonomous worker. Real process-kill recovery and V3 Sandbox acceptance remain
separate release gates; fixture tests are not live acceptance.

## Verification

```sh
go test ./...
go test -race ./api ./internal/store -run 'Refund|FinanceOperation' -count=1
```

`TestFinanceRefundOperationsPostgres` additionally requires
`ACCOUNTS_FINANCE_TEST_DSN` pointing to a fresh disposable local database named
`p0f_refund_test*`. It refuses non-loopback hosts and nonempty databases. Never
point this test at an environment database.
