# SignedConfig projection migration and runbook

XConnect-One exposes the canonical client projection at:

```text
GET  /api/overlay/v1/signed-config?device_id=<device-id>
POST /api/overlay/v1/signed-config/{generation}/ack
GET  /api/overlay/v1/signing-keys
```

The existing `GET /api/overlay/v1/config` and
`POST /api/overlay/v1/config/ack` responses are unchanged. Clients migrate by
capability: versions that support canonical SignedConfig use the new paths;
older clients remain on the legacy paths until their normal upgrade completes.

## Safety and persistence boundary

The projection service stores generations, signed payloads, and idempotent ACK
receipts through the account service's existing `store.Store`. PostgreSQL is
the production implementation and is wired automatically when the signing
secret is present. Transaction-scoped advisory locks serialize each
`user_uuid + device_id`, including its first projection, and database primary
and unique constraints prevent generation or config ID reuse. A process
restart therefore continues at the persisted generation and preserves ACKs.

The memory Store implements the same contract for tests and disposable local
integration only. Environment wiring refuses it unless
`OVERLAY_PROJECTION_ALLOW_MEMORY=true`. Production still fails closed with
`503 overlay_projection_unavailable` when either durable storage or the
signing secret is unavailable.

Do not set `OVERLAY_PROJECTION_ALLOW_MEMORY=true` in UAT or production.

## Local enablement

Apply `sql/migrations/2026082801_overlay_signed_config_projection.up.sql`
before enabling the endpoint. Then inject the current signing key through the
normal secret delivery mechanism:

```text
OVERLAY_SIGNING_CURRENT_PRIVATE_KEY=<base64 Ed25519 seed or private key>
OVERLAY_SIGNING_CURRENT_KEY_ID=overlay_signing_key_01
OVERLAY_SIGNING_CURRENT_NOT_BEFORE=2026-08-28T00:00:00Z
OVERLAY_SIGNING_CURRENT_NOT_AFTER=2027-08-28T00:00:00Z
OVERLAY_SIGNED_CONFIG_TTL=24h
```

The private-key variable accepts a base64-encoded 32-byte Ed25519 seed or
64-byte private key. The legacy `OVERLAY_SIGNING_PRIVATE_KEY` and
`OVERLAY_SIGNING_KEY_ID` names remain accepted during migration. A private key
must never be committed, logged, returned by an API, or stored in PostgreSQL.

For disposable memory mode only, additionally set
`OVERLAY_PROJECTION_ALLOW_MEMORY=true`.

## Key rotation and discovery

`GET /api/overlay/v1/signing-keys` is authenticated and publishes only
`key_id`, `algorithm`, the base64 Ed25519 public key, `status`, `not_before`,
and optional `not_after`. It never publishes a private key.
The response uses `Cache-Control: private, max-age=300`,
`Vary: Authorization`, and an ETag; authenticated clients may revalidate with
`If-None-Match` without allowing a shared cache to mix authorization contexts.

During rotation, inject the new current private key and retain the previous
public key in `OVERLAY_SIGNING_PREVIOUS_KEYS_JSON`:

```json
[
  {
    "key_id": "overlay_signing_key_01",
    "public_key": "<base64 32-byte Ed25519 public key>",
    "not_before": "2026-08-28T00:00:00Z",
    "not_after": "2026-09-04T00:00:00Z"
  }
]
```

New configurations are signed only by the current key. Current and previous
public keys verify configurations while their declared windows are active.
Keep a previous key published until every configuration it signed has expired,
including allowed clock skew, then remove it from the next secret revision. At
startup, every previous key must already be active and its `not_after` must be
at least the later of `now + OVERLAY_SIGNED_CONFIG_TTL` and the maximum
persisted `expires_at` for that key ID; shorter windows fail closed. The same
persisted-expiry check applies when a current key declares `not_after`. This
prevents rotation or a TTL change from invalidating an otherwise unexpired
configuration.

## Request lifecycle

1. The authenticated device must already be registered through the existing
   device endpoint. Only its WireGuard public key is stored by the controller.
2. The service derives a source revision from the device, selected gateway,
   WireGuard settings, and Xray transport inputs.
3. An unchanged, unexpired source returns the same config ID and generation.
4. A changed source advances generation by exactly one, signs the canonical
   payload with Ed25519, immediately verifies it, and then stores it.
5. ACK retries for the current config are idempotent and return the original
   receipt. ACKs for an older generation return `409 stale_generation`.
6. Missing `applied_at` uses controller time. Values more than five minutes in
   the future are rejected.

Every decoded contract rejects unknown fields and recursively rejects
`private_key`, `refresh_token`, and `vault_token`. XConnect-One v1 accepts only
`proxy_core: xray`.

The signed payload intentionally contains the opaque VLESS `auth_id`; this is
part of the existing SignedConfig security boundary. PostgreSQL volume/storage
encryption, encrypted backups, and least-privilege database roles are required.
Grant `SELECT/INSERT` on the two projection tables only to the account service
runtime role and migration role; operator/reporting roles do not need payload
access. Database statement/application logs must not record bind values or
response bodies for these routes, and backups must remain encrypted with access
audited. The payload is not copied into audit logs or auxiliary tables.
WireGuard private keys and refresh/vault tokens are never accepted by the
contract or stored.

## Signing bytes interoperability protocol

`SignedConfig.SigningBytes()` is a wire protocol shared by Go, Dart, Swift,
Kotlin, and Windows clients. Implementations must produce the UTF-8 bytes of a
compact JSON object with no insignificant whitespace and must exclude the
top-level `signature` member.

Top-level members are emitted in this exact order:

```text
schema_version, config_id, network_id, device_id, generation,
issued_at, expires_at, proxy_core, transport, wireguard
```

Nested member order is also fixed:

```text
transport: kind, loopback, remote, auth_id
loopback:  host, port
remote:    host, port, server_name
wireguard: interface_name, addresses, mtu, peers
peer:      gateway_id, public_key, allowed_ips, endpoint,
           persistent_keepalive_seconds
endpoint:  host, port
```

`persistent_keepalive_seconds` is the only optional signing member in v1 and
is omitted when its value is zero; it otherwise appears in the peer position
shown above. All other listed members are present.

Strings use RFC 8259 JSON escaping and the final byte sequence has no trailing
newline. Arrays preserve the order received in the projection. Integers use
base-10 JSON number notation. `issued_at` and `expires_at` are UTC RFC3339 with
whole-second precision, exactly `YYYY-MM-DDTHH:MM:SSZ`; fractional seconds and
numeric UTC offsets are rejected by contract validation.

The reusable vector at
`tests/fixtures/overlay/signed-config-ed25519-vector.json` contains a fixed
development-only seed, public key, signing payload, and expected signature.
Every platform implementation must pass that vector. The vector seed is test
material and must never be used as a deployment signing key.

## Verification

Before enabling production traffic, verify:

- two unchanged GET requests return the same generation and config ID;
- a gateway or device projection change increments generation by one;
- the Ed25519 signature verifies over `SignedConfig.SigningBytes()`;
- repeated ACK returns `duplicate: true` with the original timestamps;
- an old generation ACK returns HTTP 409;
- the legacy config endpoint still returns its `revision` contract;
- restarting the service preserves generation and ACK state;
- `/signing-keys` contains current and previous public keys and no private
  material;
- a configuration signed before rotation still verifies during the previous
  key window, while new configurations use the new key ID.

## Rollback

Stop routing capable clients to `/signed-config` and remove the signing secret.
The endpoint then fails closed with 503; legacy clients and the legacy config
endpoint continue to operate. Do not run the down migration during an ordinary
application rollback: retaining generations prevents replay when the feature is
enabled again. After the retention window and only when permanent removal is
approved, `2026082801_overlay_signed_config_projection.down.sql` removes the ACK
and projection tables.

## Verification boundary

Unit, contract, concurrency, SQL transaction, and migration-shape tests run in
CI. The PostgreSQL tests use the repository's SQL mock harness; deployment must
also run the restart/rotation smoke checks above against its actual managed
PostgreSQL service. The code does not claim a live database check when one is
not configured in CI.
