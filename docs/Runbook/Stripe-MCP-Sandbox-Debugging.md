# Stripe MCP in Codex: Sandbox debugging

Last updated: 2026-10-21

Use Stripe’s hosted MCP server to inspect test-mode payments and consult current Stripe documentation from Codex. This guide adds a local Codex connection; it does not add credentials or payment configuration to this repository.

## Configure Codex

Install or update Codex CLI from its trusted distribution, then add Stripe’s hosted MCP server to the local Codex configuration:

```sh
codex mcp add stripe --url https://mcp.stripe.com
codex mcp login stripe
codex mcp list
```

The first command saves the server URL in the user’s Codex configuration. The login command opens Stripe’s OAuth consent flow. In Stripe, grant access only to the intended Sandbox account and its minimum required permissions. Leave every Live account unselected. Do not put API keys, webhook signing secrets, or OAuth tokens in shell history, repository files, screenshots, or chat.

Codex, the Codex CLI, and the Codex IDE extension share the MCP configuration on the same computer. A new Codex session may be needed before newly added tools appear.

## Debug a Sandbox payment

Start with the read-only tools:

- `stripe_api_search` and `stripe_api_details` to find a Stripe API method and its parameters.
- `stripe_api_read` to retrieve a test-mode Checkout Session, subscription, invoice, event, or webhook endpoint.
- `search_stripe_documentation` to check current Stripe behavior.

Confirm the returned objects are from the intended Sandbox before using them to explain an application incident. Prefer event IDs and the smallest relevant time range; avoid retrieving unrelated customer data. Stripe’s account and event metadata should agree with the test environment used by the application.

`stripe_api_write` can make API changes through POST, PATCH, PUT, and DELETE requests. Review every proposed write and keep it in Sandbox. Do not replay events, create Checkout Sessions, change prices or webhook endpoints, issue refunds, or alter subscriptions unless the debugging task explicitly calls for that action. Stripe’s additional confirmation for some sensitive operations is not a substitute for reviewing each write.

## Disconnect

Revoke the Codex OAuth session in Stripe Dashboard → user settings → OAuth sessions. Remove the local server entry with the Codex CLI when the connection is no longer needed:

```sh
codex mcp remove stripe
```

## References

- [Stripe MCP setup, authentication, permissions, and tools](https://docs.stripe.com/mcp?agent=codex)
- [Stripe API keys and test mode](https://docs.stripe.com/keys)
