# Durable device credential and short session

This runbook covers the XConnect-One v1 device credential (`xdc_`) and the
short device session (`xenr_`). It does not change SignedConfig v1 or add a
second proxy runtime; Xray remains the only proxy core.

## Security boundary

Join exchange atomically registers the device and creates one active durable
credential. The raw value has the exact form
`xdc_<32-lowercase-hex>.<43-canonical-base64url>`; the suffix decodes to 32
bytes with no padding and must re-encode byte-for-byte. `credential_id` is
`xdcid_` plus the same hexadecimal segment. The raw value is returned only in
the HTTPS join response with `Cache-Control: no-store`.

Accounts stores SHA-256 of the UTF-8 bytes of the complete raw credential,
plus its id, user/network/device binding, three fixed scopes, state, expiry and
replacement chain. Verifier comparison is constant-time after id lookup. Raw
credentials, verifier values, WireGuard private keys, refresh tokens, VLESS
authentication values and signing private keys must not enter logs, audit
details, API errors or backup exports.

Only `Authorization: Device <xdc_...>` is accepted. Clients emit `Device`; the
HTTP scheme comparison is ASCII case-insensitive. Bearer, Basic, cookies,
query parameters and custom headers do not authenticate a device. All three
routes require an actual TLS request. A trusted reverse proxy may supply
`X-Forwarded-Proto: https` only when the service is explicitly started with
`OVERLAY_TRUST_FORWARDED_HTTPS=true`; production deployments must otherwise
fail closed.

## Wire flow

`POST /api/overlay/v1/device/session` accepts a canonical UUID `client_nonce`,
echoes it exactly and returns a bearer valid for ten minutes (the contract
maximum is fifteen). Its only scopes are `overlay:config:read` and
`overlay:config:ack`. The response includes the public current/previous
Ed25519 signing-key ring used by join and SignedConfig verification. Session
issuance fails with 503 if no trust root is configured. The durable credential
never reads configuration directly.

`POST /api/overlay/v1/device/credential/rotate` accepts a client-generated
successor id and SHA-256 verifier; Accounts never receives the raw successor.
`Idempotency-Key` is `sha256-<hex>` over compact JSON in the field order
`new_credential_id,new_credential_sha256`. Replacement is one transaction,
there is at most one active credential per device, and the response contains
neither raw secret nor verifier. After a lost response, the client proves the
pending successor by minting a session. Credentials live for thirty days and
the database rejects lifetimes over thirty-one days.

`POST /api/overlay/v1/device/revoke` binds a UUID nonce and the same canonical
request hash. It atomically revokes the device and all active credentials,
invalidates short sessions and join enrollment, and stores a terminal receipt.
Only this route may compare a replaced or revoked historical verifier, and
only to replay the exact terminal receipt. A changed nonce/body returns a
conflict; no historical credential can mint, rotate, read config or reactivate
a device. A 202 response with `policy_reconcile_pending:true` still confirms
that device revocation committed and Gateway peer projection excludes it.

## Deploy and verify

1. Back up the database with encryption and restrict the application DB role
   to the tables it needs. Verifier hashes are authentication material even
   though they cannot be used as bearer values.
2. Apply `2026082807_overlay_device_credentials.up.sql` after the Batch07
   lifecycle/key-history migrations.
3. Configure the existing Ed25519 key ring and verify `/signing-keys` returns
   exactly one current key. Do not inject a previous private key.
4. Enable the API and confirm join emits one `xdc_`, session returns the same
   device/network and a non-empty public signing-key ring, rotation invalidates
   the old credential, and revoke replay is stable.
5. Monitor `overlay.device_session.mint`,
   `overlay.device_credential.rotate`, device revoke audit events, 401/409
   rates, credential expiry and `overlay_policy_reconcile_pending` backlog.

The down migration deletes verifier tombstones and terminal receipts and is
therefore destructive. Before rollback, stop device traffic, revoke all issued
credentials, require explicit security approval, then apply the down migration
in its documented child-before-parent order. Rolling application code back
while leaving the tables intact is safer. After a destructive rollback every
device must be remotely revoked and re-enrolled with a new one-time invite.

## Known boundary

This batch supplies the durable credential producer and v1 session flow. It
does not add SignedConfig v2 policy references. Runtime convergence after a
revoke is handled by the existing Gateway snapshot and policy reconcile
workers; the terminal response deliberately reports pending state rather than
claiming an ACL generation that has not yet been compiled.
