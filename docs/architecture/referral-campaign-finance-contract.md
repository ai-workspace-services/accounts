# Episodic referral campaigns and financial facts

The existing XConnect referral flow remains a consumer of local financial
facts, not a replacement for invoices/payments/refunds. Clover settled refund
events resolve the original invoice through the local payment key and record
the same referral risk policy as historical charge-refunded events. Pending
refunds do not change rewards; successful partial refunds hold/reverse rewards
while retaining subscription access. Replays preserve a single refund and the
original reward history. Already settled rewards need explicit reconciliation,
not a second payout or deletion of the original settlement.

Future episodic campaigns should be implemented as a separate versioned
activity model, after the P0 financial/V3 acceptance gates:

- Stable campaign identity and immutable rule revision; product/plan scope,
  eligibility period, timezone, enrollment cutoff, reward currency, limits and
  refund/dispute policy are explicit data.
- Freeze the relevant rule revision and referral attribution when a customer
  qualifies. Changing or ending an activity never recalculates old invoices,
  payments, earned rewards or completed settlements from today's rules.
- Discounts use provider coupon/promotion references as adapter fields; the
  invoice's actual settled minor-unit amount remains the financial truth.
- Campaign credit, extra quota and referral commission are separate entries,
  each linked to its campaign/rule version and originating local invoice or
  payment. They do not overwrite cash-payment or usage facts.
- Eligibility, pending reward, release/hold, settlement and reversal transitions
  are auditable and idempotent. Refunds/disputes propagate through the original
  local invoice; failed/pending refunds cannot create a settled refund fact.
- Ending an activity closes new enrollment. Historical records and already
  earned liabilities remain readable and subject to their frozen policy.

Campaign-specific amounts, dates and customer eligibility must be supplied by
the activity owner before activating a new campaign. This change preserves
existing behavior; it does not activate a new activity or change the Portal UI.
