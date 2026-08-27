# XConnect-One invite enrollment runbook

The first productized join flow is intentionally one command and one public
exchange:

```text
xconnect join 'xconnect://join/<opaque-code>?controller=https%3A%2F%2Fcontroller.example'
```

The URI contains a short-lived join secret, never an account access token,
refresh token, VLESS credential, or WireGuard private key. The CLI generates
the WireGuard key locally and sends only its public key.

## Controller flow

1. An authenticated active account creates an invite with
   `POST /api/overlay/v1/join-tokens`. The controller returns the raw secret
   exactly once, inside `join_uri`, with `Cache-Control: no-store`.
2. The CLI parses the URI and calls the public, rate-limited
   `POST /api/overlay/v1/join-tokens/exchange` with the invite secret, device
   ID, normalized platform, hostname/name, and WireGuard public key.
3. One Store transaction locks the invite row, checks owner/network/device/
   platform/expiry/revocation/unused state, prevents replay, locks the network
   address pool, registers the device, exhausts the invite, creates a
   short-lived enrollment session, and writes the
   exchange audit event. A one-use invite therefore cannot have two concurrent
   winners.
4. The one-time exchange response returns a `xenr_` bearer. It is bound to the
   exact user, network, device ID, platform, and WireGuard public key. Its only
   endpoints are the `/api/overlay/v1/enrollment/{config,signed-config,...}`
   bootstrap reads and ACKs. Device registration already happened atomically
   in step 3, so there is no second registration window to replay.
5. The response includes the current public signing-key set. SignedConfig
   remains `proxy_core: xray` only.

An enrollment bearer is not accepted by account, admin, device-list, network-
list, ordinary overlay, or management APIs. Device/network mismatches fail with
403. Unknown, expired, revoked, exhausted, and replayed invite secrets share one
generic 401 response so the public endpoint is not a token oracle.

## Persistence and secret boundary

Apply `sql/migrations/2026082802_overlay_join_enrollment.up.sql` before routing
join traffic. PostgreSQL stores only 32-byte SHA-256 digests of both `xjt_` join
secrets and `xenr_` enrollment bearers. Raw secrets are never columns, audit
details, list responses, error messages, or log attributes. There is no invite
list endpoint in v1. The account service database role needs access to the two
new tables; reporting roles do not.

Join audit actions are:

```text
overlay.join_token.create
overlay.join_token.revoke
overlay.join_token.exchange
```

They contain IDs, constraints, expiry, and use counts, but no raw secret or
secret digest. Application/request logging must redact request and response
bodies on create and exchange. Backups and database volumes remain encrypted
under the same SignedConfig `auth_id` operational boundary.

## Limits and configuration

- Invite default TTL: 15 minutes; API maximum: 24 hours.
- Every invite is strictly one-time. The compatibility `remaining_uses` input
  accepts only omitted/0/1 and is normalized to 1; the first successful
  exchange always stores 0, regardless of device identity.
- Enrollment default TTL: 10 minutes; set `OVERLAY_ENROLLMENT_TTL` up to one
  hour when slow mobile provisioning requires it.
- `OVERLAY_CONTROLLER_URL` is required to create an invite. Production fails
  closed unless the URL is HTTPS. Userinfo, query, and fragment components are
  rejected instead of silently removed.
- Plain HTTP is accepted only for `localhost`/`127.0.0.1` development and test
  controllers when `OVERLAY_ALLOW_INSECURE_LOCALHOST=true`; never set that flag
  in production.
- The default exchange limiter allows 10 attempts/minute per client-IP plus
  non-secret digest prefix. Multi-replica production should inject a shared
  limiter through `WithOverlayJoinRateLimiter` and enforce an additional edge
  limit/WAF policy. Rate-limit keys and telemetry must never contain raw tokens.

## Verification

Before rollout, verify against managed PostgreSQL:

- database rows contain only fixed-length digests, never `xjt_` or `xenr_`;
- concurrent exchange of a remaining-use=1 invite has exactly one success;
- expiration, revocation, exhaustion, device/platform mismatch, and same-device
  replay reject without revealing secret state;
- exchange and device registration commit or roll back together;
- enrollment can fetch and ACK only its own config and SignedConfig;
- enrollment cannot access account/admin/list or another device/network;
- legacy account sessions and legacy config continue to work;
- request, error, audit, reverse-proxy, and database statement logs contain no
  raw invite or enrollment bearer.

Unit/race tests cover the memory state machine and SQL transaction shape. CI on
hosts without PostgreSQL must not claim a live migration/concurrency test;
deployment smoke tests must exercise the real database.

## Rollback

Stop issuing new invites and remove join/enrollment routes from ingress. Revoke
outstanding invite IDs if immediate shutdown is required. Existing account
sessions and overlay endpoints are independent. Do not drop the tables during
an ordinary application rollback: retained hashes keep already-consumed
invites non-replayable. Only after the maximum invite and enrollment retention
window, and after explicit permanent-removal approval, apply
`2026082802_overlay_join_enrollment.down.sql`.
