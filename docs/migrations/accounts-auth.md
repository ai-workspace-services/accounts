# Accounts-authenticated migration API

The migration client takes two HTTPS origins and two existing Accounts
administrator credentials. Accounts owns credential format, validity, lifetime
and revocation. The client does not mint migration JWTs, impose a token lifetime,
or read database DSNs. Use environment references instead of plaintext token
arguments so credentials do not enter command history or process arguments.

```sh
migratectl transfer-api \
  --source-url https://source.example.invalid \
  --target-url https://target.example.invalid \
  --source-token-env SOURCE_ACCOUNTS_TOKEN \
  --target-token-env TARGET_ACCOUNTS_TOKEN
```

Existing `--source-auth-kv` / `--target-auth-kv` references remain optional
alternatives to token environment references; they read the field `token`.
Neither storage option changes the Accounts credential contract.

## Service configuration

Configure each separate listener with `--accounts-auth-url <own Accounts HTTPS
origin>`. This is mutually exclusive with the legacy `--auth-key-env` mode;
there is no fallback to dedicated JWT authentication after Accounts refuses a
token. The listener uses POST `/api/admin/migration/authorize`, forwards only the
Bearer credential and requested operation, verifies TLS and refuses redirects
and environment proxy overrides. Authorization is rechecked on every request,
not cached. Plain HTTP is allowed only for an explicitly selected loopback test.

The Accounts endpoint accepts authenticated Bearer JWT/access or session tokens
through the existing Accounts authentication service. It rechecks the current
user: active root/admin only, excluding read-only/demo accounts. Ordinary users,
operators and internal service tokens are insufficient. Scope is an operation
selector (`source.read`, `target.plan`, `target.apply`), not a newly minted token
claim. Accounts decides whether the credential is still valid; the migration
listener adds no independent 15-minute limit or refresh flow.

Source and target may authorize different local administrators. The source
snapshot retains its source actor; target encrypted staging/plan/receipt access
remains bound to the target actor. Both artifacts still bind the server's exact
approved source/target audience pair and job identity. Clients do not manage
these JWT claims or need a separate migration credential. A different operator
cannot read another operator's persisted plan simply by guessing its UUID.

An administrator token does not bypass schema verification, physical database
identity, allowlisted source peers, snapshot hashes, row conflicts, target
baseline checks or the owner-issued backup/restore gate. Preview does not create
persistent business rows or receipt schema; enabling apply remains explicit.
The listener still needs its server-local database role and staging encryption
key. These are deployment inputs, never migration-client connection strings.

## Validation boundaries

Tests cover existing admin/root tokens, normal-user refusal, account disable,
revoked sessions, operation selection, redacted/no-store responses, no redirect
credential forwarding and no cached authorization. PostgreSQL17 tests transfer
synthetic data between distinct source/target administrators using ordinary
opaque credentials, reject apply without backup proof, apply with the fixture
owner's explicit proof, retry without duplicate writes, and refuse a subsequently
revoked credential. Fixture proof is not a production restore receipt.
