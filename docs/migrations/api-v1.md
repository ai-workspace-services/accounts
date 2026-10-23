# Migration API v1

Task: accepted migration API/CLI implementation, 2026-10-23.

This is an opt-in, separate `migratectl serve` listener. Existing Accounts API,
V4 behavior, historical SQL migrations and legacy SSH migration remain unchanged.
The endpoint must be routed explicitly by the deployment owner; never fall back
from an unsupported migration endpoint to another migration mechanism.

## HTTP contract

All routes are relative to `/api/migrations/v1`. HTTPS is required except explicit
loopback development mode. All requests require `Authorization: Bearer <token>`.
JSON requests reject unknown fields. Responses and logs never contain row data
except the authenticated snapshot stream. Every response is `Cache-Control: no-store`.

| Method/path | Scope | Body/result |
| --- | --- | --- |
| POST `/snapshots` | `source.read` | `{request_id}` -> immutable manifest |
| GET `/snapshots/{id}/manifest` | `source.read` | manifest |
| GET `/snapshots/{id}/stream` | `source.read` | NDJSON: manifest record then table/row records |
| POST `/plans/preview` | `target.plan` | `{request_id,source_url,snapshot_id,source_token}` -> plan |
| POST `/plans/{id}/apply` | `target.apply` | `{plan_sha256}` -> committed receipt |
| GET `/plans/{id}` | `target.plan` | persisted plan/committed receipt |

These are the exact JWT claim names and the GitOps declaration scope names;
there is no implicit policy-label translation. `target.plan` includes status.
JWTs additionally bind literal `source_audience`, `target_audience`, and `job_id`
claims, checked against the listener's explicit configuration. The source
manifest repeats that pair/job plus the authenticated subject's SHA-256. Target
checks this binding before fetching any rows. Both tokens must belong to the same
opaque operator; a source token from another operator/job cannot be paired with
the target token. Configure a separate listener/job for another migration pair.

IDs are caller-generated canonical UUIDs (`request_id` becomes snapshot/plan ID).
Reusing an ID with different input is a conflict. IDs are owned by the JWT subject;
recovery tokens must retain that subject. Preview retries reuse the stored snapshot
and plan even if the live source changes. Source credentials are forwarded only to
an exactly configured peer base URL, never persisted, and never sent on redirects.

Manifest fields: `format`, `scope`, `snapshot_id`, `tool_sha256`, `authorization`, `source_identity_sha256`,
`source_schema_sha256`, `snapshot_sha256`, `created_at`, `expires_at`, `tables`
(per-table rows/SHA-256), `columns` (complete per-table field/type/null/generated
coverage). Plan fields: `plan_id`, `plan_sha256`, `tool_sha256`, `state`,
`snapshot`, `target_identity_sha256`, `target_schema_sha256`,
`target_baseline_sha256`, `insert_rows`, `matching_rows`, `conflict_rows`,
`created_at`, `expires_at`. Apply accepts only the exact reviewed plan hash.

Operational/unique-constraint conflicts are HTTP 409 with an aggregate error code;
primary-key content conflicts return a persisted HTTP 200 preview with
`state=conflict`, nonzero `conflict_rows` and no apply eligibility. No identifying
values or driver errors are returned. Authentication errors are 401/403, expired
objects 410, malformed/oversized JSON requests 400, unsupported endpoints 404/405.
A busy listener returns 503; retry the same request IDs. The client explicitly
reports 404/405/501 as unsupported without fallback.

## Data and recovery contract

The reviewed scope is identity (full users including password/MFA/proxy UUID,
OAuth identities, MFA recovery codes, lifecycle events) plus subscription,
quota, billing, teams/membership/enrollment, traffic checkpoints, Stripe deduplication, finance facts and
referral dependencies. Sessions, OAuth exchange codes and recovery challenges
are excluded. No email/UUID remapping, overwrite, upsert or balance addition.
Same primary key and canonical complete content is a no-op; different content
or another unique-key collision rejects the entire operation.

Source uses one read-only repeatable-read transaction. The schema fingerprint
and canonical row hashing reuse `internal/migrate`'s reviewed catalog/semantics.
Snapshots and plans are AES-256-GCM encrypted, authenticated with object type,
ID and subject, atomically persisted under a private staging directory. Keys
come from environment references and must survive listener restart.

Apply locks the reviewed target tables, checks physical database identity,
schema/migration fingerprint and full target baseline, inserts missing rows,
verifies the complete resulting scope and records success in the same database
transaction. A receipt in the separate `accounts_migration_api` schema makes
retry after commit or lost response recoverable. Preview-only startup performs
no receipt DDL. Receipt schema initialization requires explicit
`--initialize-receipts --receipt-owner-role <exact migration DB role>` and a
verified backup gate; it is a new private schema, with no historical SQL edits.
Target-only rows are preserved. Source and target on the same physical PostgreSQL
database are refused even behind connection aliases. The DB role needs SELECT,
full visibility and EXECUTE on `pg_control_system()` to prove physical identity.
The source must use the dedicated `readonly_release` connection with no write,
sequence, role membership or bypass privileges. The server never substitutes
business credentials or grants database privileges. RLS requires the existing
reviewed `release_initialization_readonly` SELECT policy with no restrictive
policy that could hide rows. Missing visibility fails closed.

## Opt-in listener and client

The migration owner provisions distinct source readonly and target migration DB
connections, per-listener signing keys, independent staging keys, persistent
private staging directories, and explicit peer origins/CIDRs. Keys are base64
32-byte values, supplied by environment reference. Keep signing keys independent
of service and normal Accounts JWT keys. Do not deploy these example commands
without the owner's normal backup, identity and routing review.

```sh
migratectl serve --role source --audience source-migration \
  --source-audience source-migration --target-audience target-migration --job-id <job-UUID> \
  --dsn-env SOURCE_READONLY_DSN --auth-key-env SOURCE_MIGRATION_SIGNING_KEY \
  --staging-key-env SOURCE_MIGRATION_STAGING_KEY --staging-dir /private/source-stage \
  --listen 127.0.0.1:9097 --allow-loopback-http

migratectl serve --role target --audience target-migration \
  --source-audience source-migration --target-audience target-migration --job-id <same-job-UUID> \
  --dsn-env TARGET_MIGRATION_DSN --auth-key-env TARGET_MIGRATION_SIGNING_KEY \
  --staging-key-env TARGET_MIGRATION_STAGING_KEY --staging-dir /private/target-stage \
  --allowed-source-url https://source.example.invalid --allowed-peer-cidr 192.0.2.0/24 \
  --listen 127.0.0.1:9098 --allow-loopback-http
```

These loopback listeners require the owner to terminate trusted HTTPS at a local
proxy. For direct TLS use `--tls-cert` and `--tls-key`. Outbound source requests
always verify TLS, disable environment proxies, pin resolved addresses to explicit
CIDRs and refuse redirects. Source token audiences bind the exact source listener.

`migratectl migration-token --auth-key-env <key reference> --audience <listener>
--source-audience <source> --target-audience <target> --job-id <job UUID>
--subject <opaque operator ID> --scopes source.read --ttl 10m` mints a token to
stdout for an owner-controlled secret channel. Maximum lifetime is 15 minutes.
Use `target.plan` for preview/status and separately grant `target.apply` only when
needed. Refresh tokens with the same subject to resume. No HTTP token-mint route.

```sh
migratectl transfer-api --source-url https://source.example.invalid \
  --target-url https://target.example.invalid \
  --source-token-env SOURCE_MIGRATION_TOKEN --target-token-env TARGET_MIGRATION_TOKEN
# Repeat with --apply --plan-id <reviewed UUID> --plan-sha256 <reviewed hash>.
# Recovery: --status --target-url <origin> --target-token-env <ref> --plan-id <UUID>.
```

`--source-auth-kv` / `--target-auth-kv` resolve the Vault KV `token` field in memory
using the operator's existing Vault identity. Each is mutually exclusive with
its token-env counterpart. No plaintext token flag exists. Optional caller UUIDs
`--snapshot-id` and `--plan-id` allow retrying a preview after a lost response.
The public `migrate-core-data.sh` delegates to this exact command and never installs
or downloads a binary or falls back to SSH.
Before using that caller, install the Accounts release binary at a protected
absolute path and verify its release manifest through the normal release owner.
Set `MIGRATECTL_BIN` to that path and `MIGRATECTL_SHA256` to the manifest's expected
SHA-256. The wrapper verifies the existing executable before every invocation,
including `--help`; an arbitrary PATH binary is never accepted. It preserves
only explicitly referenced token variables and standard Vault/TLS configuration,
and clears loader/proxy overrides. Snapshot/plan `tool_sha256` fields bind the
actual source/target executables; a changed target executor requires a new preview.

## Verified backup gate

Target startup defaults to preview/status only. To enable new writes, the migration
owner must supply `--backup-receipt-file <absolute private file>`. This file is
read at startup and again before each new apply; HTTP callers cannot supply it.
The owner independently verifies the encrypted backup artifact and a populated
isolated restore before writing this assertion (mode 0600, no symlink):

```json
{
  "format": 1,
  "backup_id": "opaque-owner-backup-id",
  "backup_sha256": "<64 lowercase hex>",
  "target_identity_sha256": "<exact preview target identity>",
  "target_schema_sha256": "<exact preview target schema>",
  "target_baseline_sha256": "<exact preview target baseline>",
  "restored_baseline_sha256": "<independently restored identical baseline>",
  "restore_verified": true,
  "verified_at": "<UTC RFC3339>",
  "expires_at": "<UTC RFC3339 no more than 24 hours later>"
}
```

Receipt fingerprints must match the exact plan target. Expiry, baseline change,
missing/invalid proof or mismatch blocks apply. The Playbook owner must enforce
artifact decryptability, restore validation and file ownership when issuing this
receipt; this API does not infer backup success from a filename or claim to run
a restore. Embedders may inject the same verified `MigrationBackupGate`. Existing
committed receipts remain readable/retryable after backup proof expiry because
those requests perform no business writes.

## Bounded v1 and validation

V1 supports at most 32 MiB of serialized row data per captured scope and 64 MiB
per response/encrypted object. Per-row/page limits also reuse existing migration
budgets. Each listener materializes one authenticated operation at a time. These
are serialized payload budgets, not a claim of 64 MiB process RSS. Larger migrations
fail before target apply. The NDJSON stream is bounded
transport, not arbitrary-size chunk persistence. Resume is at immutable snapshot
and plan IDs; byte/chunk resume is not implemented in v1. Encrypted expired staging
files require owner-managed retention cleanup; expiration prevents new use.

Run isolated integration tests with `MIGRATECTL_API_TEST_DSN` pointing to a dedicated
loopback PostgreSQL17 `postgres` database. Tests create only randomly named new
`migration_api_test_*` databases, populate every scoped table, and clean up those
databases. They do not read any application DSN or use existing service databases.

Deployment integration is a separate commit. Neither this API nor its CLI stops
writers, changes traffic, deploys, merges or approves a cutover. Operators must
retain independently verified backups before applying to an existing target.
