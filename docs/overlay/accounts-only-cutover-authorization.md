# Accounts-only cutover authorization producer

Accounts issues the controller-signed authorization consumed by the Gateway
Batch06 accounts-only readiness verifier. This is an internal management API:

`POST /api/internal/overlay/v1/nodes/{node_id}/cutover-authorizations`

It requires `X-Service-Token`, `Content-Type: application/json`, and
`{"requested_mode":"accounts-only"}`. The response is `Cache-Control:
no-store`, is logged to the audit trail, and may last at most 15 minutes.

The signing key is deliberately separate from client SignedConfig and Gateway
snapshot keys. Configure only the private signing material in Accounts:

```
OVERLAY_CUTOVER_AUTHORIZATION_PRIVATE_KEY=<base64 32-byte seed or 64-byte Ed25519 private key>
OVERLAY_CUTOVER_AUTHORIZATION_KEY_ID=<pinned key id>
```

The Gateway receives only the corresponding public key through its protected
deployment path. Accounts never returns that private material in an API
response or signing-key discovery response.

The caller supplies no baseline, snapshot, policy, projection digest, or
reconcile counters. Accounts derives all signed values from durable records and
refuses issuance unless the node is an explicitly apply-authorized gateway,
the latest stored GatewaySnapshot is valid, a reviewed static-import receipt
exists for the same network, and no policy reconciliation is pending. The
projection digest is the compact JSON SHA-256 of the exact sorted peers in the
stored signed snapshot. The response binds `accounts-only`, node/network,
generation/snapshot, baseline/projection/policy SHA-256 values, four reconcile
counters, and a whole-second UTC validity window.

The byte sequence excludes `signature` and has the fixed order implemented in
`internal/overlay/cutoverauth`. Its golden test is intentionally equal to the
Gateway Batch06 vector. Any unavailable store, missing signer, incomplete
evidence, invalid stored snapshot, unapproved node mode, or pending
reconciliation returns an error without a signature.

This endpoint provides only the controller authorization component. The
operations pipeline must still assemble the protected readiness bundle and the
Gateway verifier independently checks import-document equality, projection,
policy artifact, runtime apply/readback, and health samples.
