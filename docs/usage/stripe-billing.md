# Stripe Billing

`accounts.svc.plus` is the server-side owner of Stripe billing.

It now provides:

- `POST /api/auth/stripe/checkout`
- `POST /api/auth/stripe/portal`
- `POST /api/billing/stripe/webhook`

## Required Environment Variables

Set these before starting the service:

```bash
STRIPE_SECRET_KEY=sk_test_xxx
STRIPE_WEBHOOK_SECRET=whsec_xxx
STRIPE_ALLOWED_PRICE_IDS=price_xstream_paygo,price_xstream_subscription
```

`STRIPE_ALLOWED_PRICE_IDS` is optional but recommended. When set, the checkout endpoint rejects any `price_id` that is not explicitly allowed.

## Create Sandbox Stripe Secrets

Use the Stripe Dashboard in **Sandbox/Test mode**. Sandbox API keys and live
API keys are separate; use only `sk_test_...` for UAT and keep it on the
server. Stripe's API key page is [Developers → API keys](https://dashboard.stripe.com/test/apikeys).

### `STRIPE_SECRET_KEY`

1. Open the [Sandbox API keys page](https://dashboard.stripe.com/test/apikeys).
2. In **Standard keys**, find **Secret key** and choose **Reveal test key**.
3. Copy the value beginning with `sk_test_` into the environment-specific Vault
   field. Do not copy a `pk_test_` publishable key; this service calls Stripe
   server-side and needs the secret key.

### `STRIPE_WEBHOOK_SECRET`

1. Open [Stripe Webhooks](https://dashboard.stripe.com/test/webhooks) while
   Sandbox/Test mode is selected.
2. Create or open the endpoint for the deployed Accounts service:
   `https://accounts-<env>.onwalk.net/api/billing/stripe/webhook`.
3. Subscribe to the events listed in [Webhook Notes](#webhook-notes), then open
   **Signing secret** and choose **Reveal**.
4. Copy the value beginning with `whsec_` into Vault. This is not the same as
   the API secret key and not the local Stripe CLI secret.

For local development, `stripe listen --forward-to ...` prints a temporary
`whsec_...`; use that only for the local process. The deployed UAT endpoint
must use the signing secret belonging to the Dashboard endpoint itself.

### Production / Live mode

Production uses a separate Stripe mode and a separate pair of secrets:

1. Switch the Stripe Dashboard from Sandbox/Test mode to **Live mode**.
2. On the [Live API keys page](https://dashboard.stripe.com/apikeys), reveal
   the production **Secret key** and copy the value beginning with `sk_live_`.
3. On [Live Webhooks](https://dashboard.stripe.com/webhooks), create or open
   the production Accounts endpoint and subscribe to the same billing events.
4. Reveal that endpoint's **Signing secret** and copy the value beginning with
   `whsec_`. It is different from the Sandbox endpoint secret, even when the
   endpoint path is the same.
5. Store both values under the production Vault fields below, then deploy or
   restart Accounts so the runtime mapping takes effect.

Do not copy `sk_test_...` or the Sandbox `whsec_...` into production. A
production webhook endpoint must use its own Live-mode signing secret. The
local Stripe CLI `whsec_...` remains local-only in both modes.

### Vault mapping

Use separate Sandbox and production fields so a mode switch cannot silently
mix credentials:

```text
kv/uat/billing-service/SANDBOX_STRIPE_SECRET_KEY  -> STRIPE_SECRET_KEY
kv/uat/billing-service/SANDBOX_STRIPE_WEBHOOK_SECRET -> STRIPE_WEBHOOK_SECRET
kv/prod/billing-service/PROD_STRIPE_SECRET_KEY   -> STRIPE_SECRET_KEY
kv/prod/billing-service/PROD_STRIPE_WEBHOOK_SECRET -> STRIPE_WEBHOOK_SECRET
```

The deployment layer performs this mapping; do not rename the runtime
variables in the Accounts service.

## Local Test Mode Runbook

1. Start the account service with Stripe test-mode credentials.
2. Expose the service so Stripe webhooks can reach it, or use the Stripe CLI:

```bash
stripe listen --forward-to http://127.0.0.1:8080/api/billing/stripe/webhook
```

3. Copy the webhook secret printed by Stripe CLI into `STRIPE_WEBHOOK_SECRET`.
4. Restart `accounts.svc.plus`.
5. Sign in through the console and start a checkout flow from a plan published
   by the Accounts billing catalog.
6. Complete the payment with Stripe test card data.
7. Verify:
   - checkout redirects back to the console
   - webhook delivery succeeds
   - `GET /api/auth/subscriptions` contains a `provider = stripe` record
   - Stripe portal opens for the same user

## Webhook Notes

The webhook currently handles these events:

- `checkout.session.completed`
- `customer.subscription.created`
- `customer.subscription.updated`
- `customer.subscription.deleted`
- `invoice.paid`
- `invoice.payment_failed`
- `charge.refunded`

The webhook is the authoritative source for Stripe subscription status in the local `subscriptions` store.

## Operational Notes

- Keep Stripe secret values server-side only.
- Use test mode until the complete flow is verified end to end.
- If checkout succeeds but no subscription record appears, inspect webhook delivery first.
