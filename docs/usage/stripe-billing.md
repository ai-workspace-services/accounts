# Stripe Billing

`accounts.svc.plus` is the server-side owner of Stripe billing.

It now provides:

- `POST /api/auth/stripe/checkout`
- `GET /api/auth/stripe/pay`
- `POST /api/auth/stripe/portal`
- `POST /api/billing/stripe/webhook`

## Required Environment Variables

Set these before starting the service:

```bash
STRIPE_SECRET_KEY=sk_test_xxx
STRIPE_WEBHOOK_SECRET=whsec_xxx
STRIPE_XCONNECT_PAY_URL=https://buy.stripe.com/test_<payment-link-id>
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
5. Start `console.svc.plus` with matching public `NEXT_PUBLIC_STRIPE_PRICE_*` values.
6. Sign in through the console and start a checkout flow.
7. Complete the payment with Stripe test card data.
8. Verify:
   - checkout redirects back to the console
   - webhook delivery succeeds
   - `GET /api/auth/subscriptions` contains a `provider = stripe` record
   - Stripe portal opens for the same user

## Direct Payment Link

When a direct link is required:

1. In [Stripe Payment Links](https://dashboard.stripe.com/test/payment-links),
   create a link for the fixed product/price.
2. In the link's payment method settings, enable the methods that the Sandbox
   account should offer. Stripe Checkout can then present the eligible card,
   Link, Apple Pay/Google Pay, and other configured methods.
3. Configure `STRIPE_XCONNECT_PAY_URL` with the raw Payment Link URL (not
   Markdown syntax).
4. Configure the Payment Link's completion behavior to return to the console,
   if the product needs a custom success page.

The authenticated route `GET /api/auth/stripe/pay`
redirects to that link and adds Stripe's documented `client_reference_id` and
`prefilled_email` URL parameters. The webhook uses `client_reference_id` to
reconcile the completed Checkout Session to the logged-in account when Payment
Link metadata is not present.

The direct link is intended for a fixed Payment Link product. For per-plan
dynamic prices, server-side metadata, or a subscription/paygo choice, continue
using the Checkout Session endpoint above. The application does not hard-code a
single payment method or expose Stripe secret keys to the browser.

## Webhook Notes

The webhook currently handles these events:

- `checkout.session.completed`
- `customer.subscription.created`
- `customer.subscription.updated`
- `customer.subscription.deleted`
- `invoice.paid`
- `invoice.payment_failed`

The webhook is the authoritative source for Stripe subscription status in the local `subscriptions` store.

## Operational Notes

- Keep Stripe secret values server-side only.
- Use test mode until the complete flow is verified end to end.
- If checkout succeeds but no subscription record appears, inspect webhook delivery first.
