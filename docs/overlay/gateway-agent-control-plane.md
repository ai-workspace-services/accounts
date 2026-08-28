# Gateway Agent control plane

The original Batch 05 contract remains shadow by default. The device lifecycle
extension adds a trusted `overlay_nodes.gateway_mode` authorization. A shared
service-token bootstrap may set it to `apply`; an `xgn_` bearer cannot promote
itself. Apply reports use `applied`, `apply_rejected`,
`apply_failed_rolled_back`, or `apply_failed_rollback_failed`; only `applied`
advances the controller-known generation. See
[device-lifecycle.md](device-lifecycle.md) for admission and rollback rules.

## Authentication and bootstrap

The legacy `POST /api/internal/overlay/nodes/heartbeat` remains the trusted
bootstrap path for public node metadata. Gateway Agent v1 never sends the
shared `X-Service-Token` to its heartbeat, snapshot, or result endpoints.

After bootstrap, an operator in the existing service-token boundary creates a
node credential with
`POST /api/internal/overlay/v1/nodes/{node_id}/credentials`. The raw `xgn_`
bearer is returned exactly once with `Cache-Control: no-store`. PostgreSQL and
memory stores retain only its SHA-256 digest. Credentials are node-bound,
expiring, and revocable through
`DELETE /api/internal/overlay/v1/nodes/{node_id}/credentials/{credential_id}`.
There is no credential-list response containing raw values.
Ingress and application logging must redact `Authorization`, credential create
responses, and credential digests. The service database role should receive
only the required DML on these overlay tables; reporting roles should not read
credential digests. Database volumes and backups remain encrypted.

The Agent uses the bearer only for:

- `POST /api/internal/overlay/v1/nodes/heartbeat`;
- `GET /api/internal/overlay/v1/nodes/{node_id}/snapshot`;
- `POST /api/internal/overlay/v1/nodes/{node_id}/apply-result`.

Path and body node IDs must match the authenticated credential. Requests use
strict JSON, bounded bodies, and `Content-Type: application/json`; snapshots
are returned as `application/vnd.xconnect.gateway.v1+json`. Responses are
`no-store`. Unknown, expired, and revoked bearers share one generic 401.

## Projection, generations, and safety

The controller deterministically projects peers from `overlay_devices` and
public node metadata. Static-import attachments restrict a migrated device to
the named node ID or endpoint host; dynamically enrolled devices without
attachments are projected to the network's gateways. A network projection
fails closed on duplicate device ID, WireGuard public key, or address.

PostgreSQL advisory locking plus generation/source uniqueness guarantees one
generation under concurrent requests. An unchanged source revision returns the
same signed snapshot. Each new generation names the prior observed projection
in `expected_previous_generation`; stale or replayed transitions are rejected.
Peer removal is checked against the signed `max_peer_removal_percent`. Empty
peer sets produce HTTP 204 unless an operator explicitly sets
`OVERLAY_GATEWAY_ALLOW_EMPTY_PEERS=true`; that override must be change-reviewed.
The default removal ceiling is 20 percent and can be changed with
`OVERLAY_GATEWAY_MAX_PEER_REMOVAL_PERCENT`. Authorizing an intentional transition
to zero peers requires both the empty-peer override and a reviewed 100-percent
removal ceiling; either missing gate fails closed.

Relay output is fixed to `proxy_core=xray` and `transport=vless-tls-xudp`.
Snapshots contain only a `credential_refs` identifier; legacy transport UUIDs,
auth IDs, bearer values, private keys, refresh tokens, and vault tokens never
cross this projection. The snapshot binds the active sanitized ACL enforcement
artifact generation and SHA-256 digest. The Agent fetches the exact canonical
bytes from the node-bound policy-artifact endpoint. Accounts projects policy
only; it does not claim the runtime has applied it until an apply-authorized
Agent reports success.

Apply results are accepted only when `(node_id, snapshot_id,
observed_generation)` identifies a persisted snapshot. Exact retries are
idempotent; the same identity with a different body conflicts. Diff counts and
the `equal` flag are checked for logical consistency. The controller persists
observed and applied generations separately. Shadow nodes require applied zero.
Apply nodes advance only with an `applied` result for the exact snapshot. Safe
failures retain the prior successful checkpoint and may transition one-way to
`applied` on retry; `apply_failed_rollback_failed` is terminal pending manual
recovery. Immutable attempt history preserves failure evidence even when the
current snapshot result advances to success.

## Ed25519 interoperability protocol

Gateway snapshots use the same injected signing key ring as SignedConfig. The
private key never enters a table. Signing bytes are compact UTF-8 JSON with
UTC, whole-second RFC3339 times. Fields are emitted in this exact order and the
`signature` object is excluded:

```text
schema_version,snapshot_id,node_id,generation,expected_previous_generation,
issued_at,expires_at,proxy_core,safety,wireguard,relay,policy
```

The fixed seed/payload/signature golden vector in the domain tests is
byte-for-byte identical to the Gateway Agent vector. Key rotation must keep the
previous public key valid until the maximum unexpired SignedConfig or
GatewaySnapshot window returned by persistence has elapsed.

## Static group_vars migration import

`POST /api/internal/overlay/v1/imports/static-clients` stays in the
`X-Service-Token` management boundary, not node bearer middleware. It accepts
only `application/vnd.xconnect.static-client-import.v1+json` and a compact
canonical v1 document in this field order:

```text
schema_version,kind,network_id,owner_user_id,source,devices
```

The required `Idempotency-Key` is
`sha256-<hex(sha256(canonical request bytes))>`. It is permanently bound to the
body hash. Exact retries return the stable receipt; reuse for another body
conflicts. The server revalidates the lowercase UUID owner, source baseline,
sorted/unique devices, 32-byte public keys, exactly one canonical IPv4 `/32`,
tags, and attachments. Secret-like tags and unknown fields are rejected.
Import atomically upserts public device projection data, metadata, receipt, and
audit evidence. Cross-owner, cross-network, key, ID, or address conflicts fail
closed. Import never invokes a runtime.

## Rollout and rollback

Apply migrations through `2026082806_overlay_device_key_history.up.sql` before
the application; this includes the original Gateway projection schema.
Create a short-lived credential, start one Agent in shadow mode, and verify:
heartbeat 204, signed snapshot verification, stable generation retry, stored
shadow result, and duplicate-result receipt. Then exercise concurrent snapshot
and static-import tests against managed PostgreSQL; unit SQL mocks do not claim
a live database concurrency result.

Rollback by revoking node credentials and removing the three Agent routes from
ingress. Keep the snapshot, receipt, and credential-digest tables so replayed
secrets and migration receipts remain detectable. The down migration is only
for an explicitly approved permanent removal after credential and snapshot
retention windows. Legacy bootstrap and static runtime remain independent.
