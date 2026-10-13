# Referral program migration and operations

Migration `2026101301` adds the referral attribution, reward, settlement, risk,
and subscription-extension ledgers. It also appends nullable
`service_period_start` and `service_period_end` facts to `finance_invoices` so
Stripe invoice line periods remain available for historical eligibility checks.
The period columns must be both null or both set. Verified provider data may
fill a previously-null pair once; other finance invoice fields stay append-only.

The migration is additive and does not connect a payout provider. Run the
upgrade and rollback fixtures against a disposable PostgreSQL database before
release. The CI `account_lifecycle_migration` job runs those fixtures and the
required referral store and annual-eligibility PostgreSQL tests. No production
database migration is authorized by this change.

## Admin settlement workflow

The authenticated self-service overview is `GET /api/auth/referrals`. Users can
request a review by posting `{"currency":"USD"}` to
`POST /api/auth/referrals/settlements`. A request reserves its ledger items in
`pending_review`; it does not send money.

An operator with `admin.billing.money.write` may record an off-platform payout
that has actually completed with
`POST /api/auth/admin/referrals/settlements/{id}/complete` and
`{"payoutReference":"<bank/provider reference>","reason":"<review note>"}`.
This records a payment fact only. It does not initiate or authorize a transfer.
The operator must retain the external receipt and enter its stable reference.
The payout reference is unique and replaying the same reference is idempotent.

If a refund or dispute arrives while a mixed settlement is under review, the
settlement remains reserved as a risk hold. Ordinary completion is blocked.
After confirming that an off-platform payout already occurred, staff can record
that fact through
`POST /api/auth/admin/referrals/settlements/{id}/reconcile-paid` using the same
request body. Reconciliation only accepts an item with a recorded risk or
reversal, records the same payment reference, marks remaining valid rewards as
settled, and leaves reversed reward debt available for offset against future
rewards. If staff confirms no payment occurred, release the reservation with
`POST /api/auth/admin/referrals/settlements/{id}/reject` and
`{"reason":"confirmed unpaid; ..."}`. Both paths require an audit reason.

Before offering cash settlements from a US LLC to individuals in China, the
business must confirm the chosen provider's permitted use and account
eligibility, approve the referral agreement and settlement documents, and
define applicable tax reporting and withholding procedures. Wise or PayPal
eligibility is not established by this code. There is no automatic payout
integration in this release; staff recording a settlement must be recording a
transfer already completed through an approved process.

## Rollback

Before any referral attribution, reward, settlement, risk, extension, or
verified service-period data exists, the down migration can remove the additive
schema after the new Accounts binary is stopped. The down migration refuses to
delete non-empty referral facts or populated service periods.

Once extension ledger rows exist, do not roll back to an Accounts binary that
does not read that ledger: confirmed subscription time could stop being honored.
Keep the extension-reader and ledger tables in place, stop new award creation
with a forward application fix if needed, and preserve all financial records.
Portal can be rolled back independently. Never remove or rewrite financial or
entitlement rows to make an old binary deployable.
