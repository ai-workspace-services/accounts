## Goal
Complete existing XConnect referral integration without visual UI changes; verify isolated PostgreSQL and real Stripe Sandbox.

## Agreed rules
- Campaign dates are configurable and administrators can define, open and close the activity. The original Asia/Shanghai [2026-10-13 00:00, 2027-01-01 00:00) window is an editable compatibility default, not a permanent restriction. Eligibility uses the rule revision effective at verified paid_at. Closing does not erase earned rewards or reject delayed events paid before closure.
- Every registered inviter can earn 10% of the first valid paid XConnect monthly/yearly subscription; integer minor units, rounded down, per currency. Exclude zero payments, PAYG and other products.
- Attribution is bound once at registration; no retrospective binding, self-referral or browser-controlled reward facts.
- Grant 30 days only when the first valid subscription payment is annual and inviter has verified valid paid subscription at that instant. Monthly-to-annual and later inviter renewal do not qualify retroactively.
- Bonus extends service tail and survives renewals; Stripe billing date remains unchanged.
- Preserve existing 7-day hold, conservative successful-refund/dispute reversal and manual off-platform settlement recording. No automatic transfer.

## Tasks
- [x] Add persistent versioned activity configuration, authorized define/open/close operations, historical payment-time eligibility and reward rule provenance; preserve existing Portal UI.
- [x] Fix delayed/non-latest invoice and pending-update paths skipping referral processing; preserve core entitlement restoration and retry safety.
- [x] Add bounded idempotent missed-reward reconciliation with preview and explicit apply.
- [x] Align annual monthly quota with earned extension access, including past_due.
- [x] Map PostgreSQL invitation-code collision for safe retry.
- [x] Preserve safe registration/login/MFA return destination and cross-domain referral parameters with UI unchanged.
- [x] Verify refund/dispute, currency, settlement reservation and API authorization contracts.

## Acceptance
- [x] Configure different/open-ended dates, open/close, unauthorized mutation rejection, and historical-rule retention. Payment before close delivered after close still qualifies; payment after close does not. Reopening does not rewrite old rewards.
- [x] Full webhook entry: delayed first payment after renewal, pending update, duplicate events and transient retry award exactly once.
- [x] Campaign boundary and late-delivery checks use paid_at.
- [x] Unpaid inviter gets cash; annual days require payment-time eligibility; monthly then annual does not double award.
- [x] Renewals preserve exact bonus days; past_due reward tail includes access and annual monthly quota, followed by expiry.
- [x] Real PostgreSQL migration, transaction and concurrency tests pass, including code collision and settlement idempotency.
- [x] Refund/dispute before/after payment and after settlement reservation preserve history and prevent duplicate payout.
- [ ] Referral link -> registration -> verification/login/MFA -> invitation center retains attribution/destination; visual UI unchanged.
- [x] Real Stripe Sandbox payment -> signed webhook -> local finance -> reward -> entitlement -> center API; refund/replay verified. Simulated tests recorded separately.

## Delivery
Separate Accounts and Portal worktrees and linked PRs. Preserve unrelated changes. Record exact source SHAs, commands/results and any Sandbox access blocker. No merge, production migration or deployment. Keep acceptance items open until their evidence is complete.

The repository has disabled Issues, so this checked-in task is the equivalent requirement and acceptance record. Base Accounts commit: `3ef09c82f46152096bfc16edc9ba767f128a197f`. Portal and frontend-router have separate companion branches named `feature/referral-completion-20261025`.

## Activity operations

All administration requires the existing `admin.billing.money.write` permission. The activity remains one configurable XConnect campaign; each change appends a server-timestamped revision with actor and reason.

| Method and path | Behavior |
| --- | --- |
| `GET /api/auth/admin/referrals/campaign` | Read current configuration and audit identity. |
| `PUT /api/auth/admin/referrals/campaign` | Define complete desired configuration: required `enabled`, `reason`, nullable `startsAt` and `endsAt` in RFC3339. Null/omitted bounds mean unbounded. End is exclusive. |
| `POST /api/auth/admin/referrals/campaign/open` | Body `{ "reason": "..." }`; open while preserving configured dates. A future scheduled start still applies. |
| `POST /api/auth/admin/referrals/campaign/close` | Body `{ "reason": "..." }`; close while preserving configured dates. |
| `POST /api/auth/admin/referrals/reconcile` | Preview up to 50 explicit `invoiceIds` for `accountUuid`; `apply` defaults false. Applying requires `apply:true` and an audit `reason`; every invoice is reverified and financial/reward facts are idempotent. |
| `GET /api/referrals/campaign` | Anonymous, uncached public status/dates/rates; excludes actor and reason. |

The original October–December dates are only initial migration defaults. Closed activity does not block ordinary registration, historical reward reads, review, settlement or reversal. Paid events are evaluated with the revision effective at `paid_at`; reopening never rewrites earlier rewards. Portal uses the public contract in its existing layout, refreshing every 30 seconds/on focus; Accounts enforces eligibility independently.

## Financial and migration compatibility

Finance invoices/payments/refunds retain Stripe provider integer units; rewards separately normalize ISO units (USD/ISK/UGX/MGA signed payment/refund/replay regressions passed). Historical inconsistent units are rejected without rewriting facts. Generic settled invoices retain their immutable description and issuance time. Verified invoice-price evidence is appended to `referral_verified_invoices`; current subscription metadata cannot qualify an unverified historical generic payment. Reward snapshots bind to immutable campaign revisions. Three additive tables are included in native schema, full-business copy and chunk-transfer contracts. Constraints/triggers stay enabled; an automatically generated snapshot may be accepted during copy only when it exactly equals the source.

Migration `2026102501` is qualified only on disposable local databases here. Once activity history, verification facts or reward snapshots exist, destructive down migration is refused. Preserve ledger readers and use forward fixes after awards exist.

## Evidence and remaining acceptance

- `go test ./... -count=1` passed across the Accounts repository. Opt-in database/Sandbox checks are recorded separately below.
- Real local PostgreSQL: original referral ledger/concurrency/settlement regression, code-collision handling, nine annual qualification scenarios and versioned campaign history passed with `referral_app` (no superuser, role creation, database creation, replication or RLS bypass).
- Full native-schema PostgreSQL webhook regression passed with a deterministic Stripe adapter: signed entry, generic settled invoice/issued time preserved, exact 10% and 30 days, immutable price evidence, wrong product/amount/conflicting evidence rejection. This is **not** real Stripe Sandbox evidence.
- Fixture-specific RLS policies authorize business DML only on the necessary synthetic financial/lifecycle tables; RLS is retained. Separate audit identity can read aggregate rewards and is denied raw user credentials and writes. These are local fixture assertions, not production identity acceptance.
- Native initialization, full-business copy and chunk-transfer qualification ran against PostgreSQL 17; exact commands and final schema digest are in `xconnect-referral-native-qualification-20261025.md`.
- Portal: 39 targeted Vitest tests, eight SSR-boundary tests, lint/typecheck and pinned-content production Webpack build passed. Frontend-router typecheck, 186 tests and deployment-contract test passed.
- Real Sandbox test is implemented as `TestReferralStripeSandboxPostgres`, opt-in with `ACCOUNTS_REFERRAL_SANDBOX=1`, test secret supplied only in process environment, and a localhost `referral_sandbox_test` database using `referral_app`. It creates dedicated inviter monthly and friend annual Sandbox transactions, reads authentic provider events, locally signs delivery into the handler, checks finance/reward/extension/overview/replay and real refund reversal. This checks local signed delivery, not public automatic Stripe webhook transport.
- After credential refresh, the real Stripe Sandbox test passed again after final currency fixes (18.69 seconds): two genuine test-mode subscriptions/payments, exactly 10% cash reward and 30 days, authenticated invitation-center read, duplicate event handling, genuine successful refund and single cash/day reversal. Events were retrieved from Stripe and locally signed; public webhook transport is not claimed. Test subscriptions are scheduled to stop at period end and financial history is retained.
- Deployed cross-domain browser email-verification/MFA and public webhook transport are not claimed by local tests; no production rollout was performed.

Stripe API preflight confirmed the requested account and unique enabled Test endpoint at the requested public webhook address, with Clover `2026-01-28.clover` and payment/subscription/refund events. Dispute events are currently absent from that endpoint subscription; no external endpoint settings were changed. Stored signing-secret presence is checked, but matching it to the public receiver is not proven by API metadata.
