# Free to paid subscription recovery

The public subscription page describes the user's plan, allowance and payment status. Provider names, callback mechanics, account keys and storage details belong in this runbook rather than purchase instructions.

## Payment and entitlement boundary

1. The server previews the selected plan and reserves one purchase operation before creating Checkout.
2. A successful browser return does not grant access. Stripe must report the Checkout/invoice paid, and a correctly signed webhook must persist the matching local subscription and allowance.
3. `subscription_create` starts a new paid allowance: an exhausted Free 5 GiB account receives the full Plus 20 GiB allowance. Existing paid plan upgrades preserve consumption and add only the allowance difference.
4. The quota grant and profile allowance marker commit in one transaction. Repeated and reordered webhook deliveries must not refill usage consumed after activation. Operator proxy pauses remain in force.
5. Once the local projection matches the provider, purchase polling releases the reservation. The Console then revalidates subscriptions, billing, usage, policy and node discovery immediately.

## Diagnose a pending paid purchase

Inspect provider state using the test API key, then compare the selected user's local purchase operation, subscription, billing profile and quota using read-only queries. Never repair this by clearing the pending operation or bypassing node authorization.

An exhausted Free profile with no matching local Stripe subscription is not a completed Plus activation, even if Checkout is paid. Check webhook deliveries first. `401 invalid_signature` occurs before projection; a successful status-poll HTTP response can still contain `pending: true`.

The webhook URL and its signing secret are a pair. Test/live mode owns credential naming; deployment generations do not. Keep the fixed Vault path `kv/prod/billing-service` and fields `SANDBOX_STRIPE_SECRET_KEY` / `SANDBOX_STRIPE_WEBHOOK_SECRET` for test mode. Never substitute `PROD_*` credentials during sandbox recovery. Reload the receiving runtime after updating the field, and resend only the affected test events through Stripe's official delivery mechanism.

The Console represents a missing optional policy snapshot as an uncached empty state. Accounts still distinguishes `404 policy_not_found` from `500 account_policy_unavailable`; route, authentication and database failures must remain visible.

## Acceptance

- Unpaid or invalid-signature events do not grant access.
- Exhausted Free to first paid Plus: 20 GiB available, Plus profile, no quota exhaustion and node discovery allowed.
- Consume 3 GiB, replay activation deliveries: 17 GiB remains.
- Existing Plus to Pro upgrade preserves the consumed usage and billing period.
- A manual proxy pause is not removed by payment.
- Payment confirmation refreshes all account read models without a page reload.
- Verify test mode, matching local subscription and completed purchase operation before declaring recovery complete.
