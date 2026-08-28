# XConnect-One dynamic ACL compiler runbook

NetworkPolicy `overlay.xconnect.svc.plus/v1alpha1` is a controller projection contract. It does not directly mutate nftables or any client runtime. The first enforcement target remains the Gateway Agent; Batch 06 publishes deterministic, deny-first artifacts for a later atomic runtime apply phase. Xray remains the only relay core.

## Safety model

- `spec.defaultAction` must be `deny`; unknown fields, subjects, services, protocols and ports fail validation.
- Rules compile in deny-first order. User/group/tag selectors are expanded to sorted device IDs using the current eligible device inventory. Inactive users and their devices are excluded. Known groups or owned tags may expand to an empty set, which safely removes access and creates a new build generation.
- Management source and explain data stay in Accounts. The Gateway artifact contains only rule ID, action, device IDs, protocol, ports and compiler-owned protected flow identifiers; it contains no email, user UUID, tag owner graph or secret.
- Only the five exact `control:*` identifiers in the enforcement schema are protected. Arbitrary `control:*` values remain denied.
- Device tag replacement is authorized by the active policy's `tagOwners`, is network-scoped and audited. Successful writes synchronously request a new build; HTTP 503 `policy_recompile_pending` means the tag write committed but artifact publication must be retried. Production should alert on this code.

## Enable and operate

Apply `2026082804_overlay_acl_compiler.up.sql`, deploy Accounts, validate a document, create a draft, inspect warnings, then activate it. Activation increments the network policy generation. Any eligible device/tag membership change detected during snapshot or artifact projection recompiles the active source, inserts an immutable build row and advances generation. Compilation or persistence errors fail the artifact/snapshot request; the previous build remains stored as LKG and the error must be alerted rather than silently treated as current.

Gateway agents fetch the exact bytes at `GET /api/internal/overlay/v1/nodes/{node_id}/policy-artifacts/{generation}/{sha256}` with their node-bound `xgn_` bearer. The response is `application/vnd.xconnect.gateway-policy.v1+json`, `Cache-Control: no-store`, and SHA-256(body) must equal both the URL digest and signed GatewaySnapshot `policy.ruleset_sha256`. The URL generation must equal signed `policy.generation`. Missing device IDs in the signed snapshot fail runtime projection closed.

Golden bytes are in `tests/fixtures/overlay/network-policy-enforcement.golden.json`; its digest is in the adjacent `.sha256` file.

## Rollback and limitations

Activate a prior revision to perform an audited rollback; any authorized policy administrator may do this because policies are network-owned, while `owner_user_uuid` records the author. To remove the feature schema, first stop policy writes and Gateway artifact reads, then run the down migration. Do not drop tables while an active deployment references their digests.

IPv4/IPv6 are family-neutral at the compiler's device-ID layer, but ACL-012 is not claimed complete: nftables `ip6` rendering and a cross-runtime IPv6 enforcement test remain Gateway runtime work. PostgreSQL behavior is covered by transactional SQL unit tests/migration shape in this repository; deployment must still run migration and concurrent activation smoke tests against the target PostgreSQL version.

Policy source can contain account identifiers. Grant the Accounts runtime DB role only the required policy-table access, encrypt backups, and never log source/artifact request bodies. Enforcement artifacts are deliberately sanitized, but management source and audit metadata remain controller-confidential.
