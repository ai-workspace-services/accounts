# XConnect-One device lifecycle

The v1 device resource is controller-owned state. Clients submit only a WireGuard **public** key; private keys and enrollment credentials are rejected by strict JSON decoding and are never stored in lifecycle events or audit details.

## State and concurrency

`active` devices are eligible for SignedConfig, enrollment, ACL inventory and Gateway peer projection. `inactive` devices remain visible but are excluded from those projections. `revoked` is terminal: revoke/leave is idempotent, invalidates device-bound join tokens and enrollment sessions, and a revoked device ID cannot be registered again.

During the short join window, the bound `xenr_` enrollment bearer has `overlay:device:revoke` and may call `POST /api/overlay/v1/enrollment/device/revoke` with an empty JSON object. It cannot name another owner, network or device. This is intentionally **not** the final long-lived CLI leave mechanism: enrollment remains short-lived and clients normally erase it after ACK. A hash-only, rotatable device refresh credential and exchange endpoint remain a Batch 08 release blocker; do not extend `xenr_` lifetime or persist it as a durable bearer.

Mutations use `expected_key_version` or `expected_state_version`. A retry carrying the already-installed public key or already-revoked state returns `duplicate: true`; a competing version returns HTTP 409. `GET /api/overlay/v1/events?after=N` is an ordered per-owner incremental stream. Administrators may provide `owner_user_id` only with settings read/write permission; ordinary users are always scoped to themselves.

Key rotation naturally changes the Gateway snapshot source document and therefore its signed snapshot generation. It does not invent an ACL build: ACL selectors are device-ID based and the enforcement artifact is unchanged. Inactive/revoked state changes synchronously call the active policy reconciler, so the next policy build removes the device. If an authenticated-user mutation persists but recompilation fails, the API returns 503 `policy_recompile_pending`; safely retry the same idempotent state request. Enrollment self-revoke cannot reuse its invalidated bearer, so it instead returns 202 with `revoked:true` and `policy_reconcile_pending:true`, and writes a durable outbox row. The service-token job `POST /api/internal/overlay/v1/reconcile-pending` retries and clears those rows; schedule it until `failed=0`. Gateway peer projection filters device state independently, so it never re-adds the device while ACL reconciliation is pending.

## Gateway apply authorization

Gateway Agent credentials are node-bound. A trusted `X-Service-Token` bootstrap heartbeat sets `gateway_mode` on the node; omitted means `shadow`. The node bearer cannot change this authorization. Apply heartbeats are accepted only for an apply-authorized node, with `applied_generation <= observed_generation` and a controller-recorded successful `applied` result for every non-zero applied generation.

Apply results use the Agent enums `applied`, `apply_rejected`, `apply_failed_rolled_back`, and `apply_failed_rollback_failed`. Only `applied` may set `runtime_applied=true` and advance the applied generation. Failure and rollback reports preserve the last known successful checkpoint. Snapshot ID, generation and node binding remain transactionally checked and conflicting retries fail closed.

## Deployment and rollback

1. Apply migration `2026082805_overlay_device_lifecycle.up.sql` before deploying the service.
2. Bootstrap nodes without `gateway_mode` to keep shadow behavior. Authorize apply per node only after Agent and controller are both upgraded.
3. Monitor `policy_recompile_pending`, stale gateway reports, lifecycle event lag and revoked-device config attempts.

The migration also adds `artifact_canonical BYTEA` beside policy JSONB. JSONB remains the query/constraint representation; the BYTEA value is the only source for digest verification and Gateway responses. Existing policy rows are deliberately left NULL because `artifact::text` is not the original compiler byte sequence. After deployment, invoke the reconcile job for each active network (normal Gateway resolution also does this) to compile source into canonical bytes and a new generation. Recreate old drafts before activation; activation fails closed while canonical bytes are absent. Generation 1 remains reserved for bootstrap default-deny, so the first real policy activates at generation 2.

Rollback is guarded. First return every node to shadow and ensure there are no apply-mode node-status/apply-result rows. Ensure no revoked public key has been reused by another active device, because the down migration restores the older all-row public-key uniqueness rule. Only then run the down migration and deploy the previous binary. Database backups must remain encrypted and access to device public keys/events limited to the Accounts runtime role.
