# Design

Use this page as the English entry point for the current design tradeoffs behind `accounts.svc.plus`.

## Primary Decision Record

The main design record is [architecture/design-decisions.md](../architecture/design-decisions.md). It captures the implementation choices that are actually true in the current codebase, including:

- session-first authentication instead of a JWT-only control plane,
- `store.Store` as the boundary around primary business persistence,
- GORM used only for specific admin-side models,
- agent pre-shared token authentication through `agentserver.Registry`,
- XWorkmate secret locator plus Vault-backed secret persistence,
- Xray config generation and periodic convergence as the control model.

## Suggested Reading Order

1. [Design decisions](../architecture/design-decisions.md)
2. [Architecture overview](../architecture/overview.md)
3. [Authentication details](../api/auth.md)

## Design Snapshot

- Runtime truth is meant to come from the current store / runtime contracts, not from duplicated local config-center state.
- Sessions and OAuth exchange codes are persisted in the database; password recovery challenges are persisted in `password_recovery_challenges`. MFA challenges and registration email verification state remain process-local.
- Admin policy and homepage customization are intentionally separated into GORM-backed services rather than folded into the primary store abstraction.
- Agent mode reuses the same Xray generation primitives as server mode instead of introducing a second configuration model.

## Related Pages

- [Architecture](architecture.md)
- [Developer Guide](developer-guide.md)
