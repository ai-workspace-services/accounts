# Stripe local financial facts and Clover compatibility

Accounts consumes signed snapshot events using Stripe API `2026-01-28.clover`.
Historical invoice snapshots remain readable. Clover subscription references
come from `parent.subscription_details.subscription`; a settled payment comes
from the expanded Invoice Payment list. A single paid PaymentIntent must match
the invoice's amount and currency. Missing, partial, paginated or ambiguous
payment evidence remains a retryable error, rather than an invented payment.
Subscription item periods populate the period for supported single-item plans.

`invoice.paid` writes one immutable local invoice and payment. Provider IDs are
external references; local UUIDs remain the financial keys. A retry completes
an interrupted write without duplicating the invoice or payment.

`refund.created` and `refund.updated` write a refund only after `succeeded`.
The provider refund ID is the idempotency key across different Stripe events.
The refund references its original local payment and inherits the existing
currency and total-refund constraints. A refund arriving before its payment
remains retryable. Pending or failed refunds do not post financial facts;
partial refunds do not revoke subscription access. Existing `charge.refunded`
handling controls full-refund entitlement revocation separately.

Sandbox Accounts rejects signed events carrying `livemode=true`. The runtime
receives only `kv/prod/billing-service` keys `SANDBOX_STRIPE_SECRET_KEY` and
`SANDBOX_STRIPE_WEBHOOK_SECRET` during the isolated V3 rehearsal. Secret values
must never enter logs, this document or the repository.

No schema migration is required: the existing schema `2026101703` already
contains the finance tables, immutable-ledger triggers and provider indexes.
The release rehearsal must preserve existing user/subscription/usage rows,
take and restore a V3 checkpoint before enabling test writes, verify duplicate
events and partial/full refunds using a dedicated test account, and record
the exact Accounts image digest and GitOps release identity. V2 and its public
domains are outside this release target.

References:

- [Invoice object](https://docs.stripe.com/api/invoices/object)
- [Invoice Payment object](https://docs.stripe.com/api/invoice-payment/object)
- [Webhook signatures and event delivery](https://docs.stripe.com/webhooks)
