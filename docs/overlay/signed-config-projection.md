# SignedConfig projection migration and runbook

XConnect-One exposes the canonical client projection at:

```text
GET  /api/overlay/v1/signed-config?device_id=<device-id>
POST /api/overlay/v1/signed-config/{generation}/ack
```

The existing `GET /api/overlay/v1/config` and
`POST /api/overlay/v1/config/ack` responses are unchanged. Clients migrate by
capability: versions that support canonical SignedConfig use the new paths;
older clients remain on the legacy paths until their normal upgrade completes.

## Safety and persistence boundary

The projection service owns monotonic generation assignment and idempotent ACK
receipts through a repository interface. This batch includes a thread-safe
memory repository for tests and local integration only. It is not production
durability: restarting the process loses generations and ACKs.

Production therefore fails closed. Environment-based wiring does not create a
memory repository unless `OVERLAY_PROJECTION_ALLOW_MEMORY=true` is explicitly
set. A production deployment must inject a durable implementation through
`WithOverlayProjectionService`; until that exists, the endpoint returns
`503 overlay_projection_unavailable` and the legacy endpoint remains usable.

Do not set `OVERLAY_PROJECTION_ALLOW_MEMORY=true` in UAT or production.

## Local enablement

Set these only in a disposable development environment:

```text
OVERLAY_PROJECTION_ALLOW_MEMORY=true
OVERLAY_SIGNING_PRIVATE_KEY=<base64 Ed25519 seed or private key>
OVERLAY_SIGNING_KEY_ID=overlay_signing_key_01
OVERLAY_SIGNED_CONFIG_TTL=24h
```

`OVERLAY_SIGNING_PRIVATE_KEY` accepts a base64-encoded 32-byte Ed25519 seed or
64-byte private key. It must come from the normal secret delivery mechanism and
must never be committed, logged, returned by an API, or stored in a projection.
The service emits only the key ID and signature.

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

Before enabling a durable implementation, verify:

- two unchanged GET requests return the same generation and config ID;
- a gateway or device projection change increments generation by one;
- the Ed25519 signature verifies over `SignedConfig.SigningBytes()`;
- repeated ACK returns `duplicate: true` with the original timestamps;
- an old generation ACK returns HTTP 409;
- the legacy config endpoint still returns its `revision` contract;
- restarting the service preserves generation and ACK state. This last check
  must fail for the development memory repository and pass before production.

## Rollback

Stop routing capable clients to `/signed-config` and remove the projection
service injection. The endpoint then fails closed with 503; legacy clients and
the legacy config endpoint continue to operate. Do not reset or delete durable
generation state during rollback, because reusing an old generation enables
replay when the feature is enabled again.

## Known production gap

A PostgreSQL repository and signing-key rotation/key-discovery endpoint are not
implemented in this batch. Memory mode is deliberately gated so this gap cannot
silently become production behavior.
