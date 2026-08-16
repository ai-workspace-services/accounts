# Multi-tenant bridge credentials — UAT release record

Release candidate: `uat-daily-build-2026.08.11-r4`

Scope: Accounts, xworkmate-bridge, xworkmate-app, and the xworkmate bridge
ingress role. This is a UAT-only upgrade record. No production account,
`root@tky-proxy.svc.plus`, Xray process, or production database is changed by
the implementation or migration commands in this record.

## Feature inventory

### Identity model

| Field | Meaning | Rotation rule |
|---|---|---|
| `users.uuid` | Permanent internal account primary key | Never changes |
| `users.email` | Login/contact address | Mutable; not a primary key |
| `tenant_id` | Current tenant access context | Selected from the resolved tenant/membership |
| `bridge_credentials.credential_uuid` | External credential identity within a tenant | UUIDv7; rotatable |
| `bridge_credentials.token_hash` | HMAC-SHA-256 digest of the user credential token | Raw token is never persisted |

Username, email, password hash, MFA secret, MFA enabled state, MFA issue time,
MFA confirmation time, and `users.uuid` are not migration inputs and are not
updated by this release.

### Xray compatibility

The existing Xray configuration is the source of truth for the first import:

```text
/usr/local/etc/xray/config.json
inbounds[].settings.clients[].id
       -> users.proxy_uuid
       -> bridge_credentials.credential_uuid
```

The client id is copied byte-for-byte as the initial credential UUID. It is
never replaced with `users.uuid`; no Xray config file is written, and Xray is
not restarted during migration. Explicit future UUID rotation updates the
active tenant credential rows together with the legacy Xray identity field so
they cannot drift.

### Authentication flow

```text
App login/session
  -> Accounts /api/auth/xworkmate/profile/sync
  -> user + tenant credential token issued
  -> token stored only in secure storage
  -> Caddy forwards Authorization without token matching
  -> Bridge calls Accounts /api/internal/bridge/credentials/introspect
  -> Accounts validates X-Service-Token and token_hash
  -> Bridge routes the task
  -> OpenClaw connection uses OPENCLAW_GATEWAY_SERVICE_TOKEN only
```

The user Authorization header is not copied into task parameters and is not
forwarded to OpenClaw. Missing introspection configuration or an unavailable
Accounts response fails closed.

### Root and rotation policy

- Multiple root users are allowed; there is no singleton root index.
- Root users can rotate credential UUIDs like other users.
- UUID rotation and token rotation are independent.
- Token sync produces a new raw token and stores only its digest.
- A UUID rotation updates active credential rows for every tenant belonging to
  the user; the existing token hash remains unchanged.

### Removed static configuration

The service code no longer uses `accounts.svc.plus` as an endpoint fallback or
`admin@svc.plus` as a root bootstrap email. Deployment must provide
`ROOT_BOOTSTRAP_EMAIL` and `XWORKMATE_BRIDGE_SERVER_URL`. Caddy and Bridge no
longer consume `BRIDGE_AUTH_TOKEN`, `BRIDGE_REVIEW_AUTH_TOKEN`, or
`AI_WORKSPACE_AUTH_TOKEN` as public ingress credentials.

The App parser temporarily accepts the old response key `BRIDGE_AUTH_TOKEN`
only when the new `bridgeCredential.token` field is absent. The compatibility
owner is the App release; remove that branch after all supported UAT builds
return the structured field.

## UAT migration procedure

1. Export a transaction-consistent database backup.
2. Record a fingerprint of `users(uuid, username, email, password,
   mfa_totp_secret, mfa_enabled, mfa_secret_issued_at, mfa_confirmed_at)`.
3. Apply the versioned migration:

   ```sh
   migratectl migrate --dsn "$UAT_ACCOUNTS_DSN" --dir sql/migrations
   ```

4. Copy the UAT Xray JSON to the migration runner. Run the dry-run import:

   ```sh
   migratectl import-xray-credentials \
     --dsn "$UAT_ACCOUNTS_DSN" \
     --tenant-id "$UAT_TENANT_ID" \
     --xray-config ./uat-xray-config.json \
     --dry-run
   ```

5. Review that every id resolves through `users.proxy_uuid`, then repeat
   without `--dry-run`. The command inserts only `bridge_credentials` rows.
6. Compare the user fingerprint. It must be identical. Verify that every
   imported row has `credential_uuid = users.proxy_uuid` for the matching
   user.
7. Configure the service secrets from the deployment secret manager:

   ```text
   Accounts: BRIDGE_CREDENTIAL_TOKEN_PEPPER, INTERNAL_SERVICE_TOKEN,
             ROOT_BOOTSTRAP_EMAIL
   Bridge:   BRIDGE_ACCOUNTS_INTROSPECTION_URL,
             BRIDGE_ACCOUNTS_SERVICE_TOKEN,
             OPENCLAW_GATEWAY_SERVICE_TOKEN
   ```

8. Deploy the four release artifacts to UAT, verify App sync, Bridge
   introspection, task routing, and OpenClaw execution. Do not edit the Xray
   client list as part of this release.

## Rollback

Before service rollout, rollback is the database backup restore procedure.
After service rollout, revert service artifacts and retain the additive
`bridge_credentials` table; its rows are not used by the old service. Do not
delete the table or rewrite `users` during rollback.

## Verification record

- Accounts: `go test ./cmd/migratectl ./internal/store ./api`
- Accounts: `go build ./cmd/accountsvc`
- Bridge: `go test ./internal/service`, `go vet ./...`, `go build ./...`
- App: `flutter analyze` and `flutter test test/runtime/runtime_controllers_settings_account_test.dart`
- Infra: `ansible-playbook --syntax-check deploy_xworkmate_bridge_vhosts.yml`
- All four worktrees passed `git diff --check` before commit.
