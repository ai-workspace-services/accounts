# XConnect-One Overlay v1 contracts

This directory records the controller-side compatibility boundary shared by
XConnect-One clients, gateways, and `accounts.svc.plus`.

## Sources of truth

- `api/openapi/overlay-v1.yaml` describes the HTTP API implemented at
  `/api/overlay/v1` and `/api/internal/overlay/v1`.
- `api/schemas/overlay/signed-config.schema.json` describes the signed
  device configuration that the controller will project for clients.
- `api/schemas/overlay/gateway-snapshot.schema.json` describes the complete
  desired state projected for a gateway.
- `internal/overlay/domain` contains the transport-neutral Go representation
  and validation rules used by controller code.
- `tests/fixtures/overlay` contains stable examples consumed by contract tests
  and downstream client/gateway implementations.

The existing unversioned Overlay routes remain available during migration.
New generated clients must use the explicit v1 paths. This baseline does not
change the legacy configuration response; SignedConfig and GatewaySnapshot are
separate versioned projection contracts so migration does not reinterpret an
existing response shape.

Canonical SignedConfig projection is available as an additive endpoint without
changing the legacy config response. See
[`signed-config-projection.md`](signed-config-projection.md) for enablement,
migration, ACK semantics, rollback, and the durable repository requirement.
The one-command invite and short-lived enrollment flow is documented in
[`join-enrollment.md`](join-enrollment.md).

## Runtime boundary

XConnect-One v1 supports only Xray-core/libXray. The signed contracts therefore
use the closed value `proxy_core: xray`. A different core identifier, including
`sing-box`, fails validation instead of triggering fallback behavior.

## Compatibility rules

- `schema_version` is exactly `1`.
- `generation` is positive and monotonically increases per network projection.
- a gateway snapshot advances `expected_previous_generation`; stale or replayed
  snapshots are rejected.
- an empty gateway peer set requires `safety.allow_empty_peers: true`, and peer
  removal is bounded by `safety.max_peer_removal_percent`.
- `expires_at` is later than `issued_at`.
- signatures use Ed25519 and carry an explicit `key_id`.
- interface addresses and allowed IPs are valid IPv4 CIDRs; WireGuard public
  keys and Ed25519 signatures have their exact encoded lengths.
- private WireGuard keys and unsealed credentials never appear in controller
  persistence or API requests.
- schema additions must be backward compatible; breaking changes require a new
  versioned path and schema identifier.
