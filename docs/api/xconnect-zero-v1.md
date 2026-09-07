# XConnect Zero v1 API

`accounts` is the only formal configuration source for XConnect Zero. The
independent `XConnect-One` CLI is the `one` controlled-client role. A
self-hosted VPS/EC2 node is the `gateway` relay/service role. Neither role
loads production configuration from the experimental lab controller.

Both roles use the same central lifecycle: a trusted bootstrap pre-seeds a
network and one-use invitation, the device exchanges the invitation, receives
hashed-at-rest credentials and a short enrollment bearer, then fetches an
Ed25519 signed configuration from Zero. The client private WireGuard key is
generated locally and is never sent to or stored by Accounts.

The One client uses `/api/overlay/v1/enrollment/signed-config` and its ACK
endpoint. A Gateway uses `/api/overlay/v1/gateway/signed-config`, which is a
separate signed snapshot containing active One peers and relay settings. It is
intended for a long-lived VPS/EC2 host with WireGuard, forwarding, and the
external Xray service; Cloud Run, Cloudflare Workers, and similar serverless
hosts are outside the Gateway boundary.

The current additive implementation is in `internal/overlay`. Its
`Repository.Seed` method is the trusted integration bootstrap path for a
pre-seeded SIT/UAT/production network and invite. Admin UI and user-managed
network/invite CRUD remain follow-up work; no unauthenticated management route
is exposed.

Responses containing credentials use `Cache-Control: no-store`. Signed
configuration uses `Cache-Control: private, no-store`, an ETag, and an
Ed25519 signature. Request JSON rejects unknown fields. Token hashes are
stored using SHA-256; raw join, device, and enrollment tokens are returned
only at issuance.
