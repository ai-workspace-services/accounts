# UAT bridge credential migration

This upgrade is additive. `users.uuid`, username, email, password hash, MFA
secret and MFA timestamps are read-only throughout the procedure.

1. Take a transaction-consistent database backup and record row fingerprints
   for `users(uuid, username, email, password, mfa_totp_secret, mfa_enabled)`.
2. Apply `migratectl migrate --dir sql/migrations`.
3. Copy the UAT Xray config to the migration runner without editing it, then
   run `migratectl import-xray-credentials --tenant-id <tenant> --xray-config <copy> --dry-run`.
4. Review that every client id resolves through `users.proxy_uuid`, then rerun
   without `--dry-run`. The command inserts only `bridge_credentials` rows
   using the client ids exactly as found in the config.
5. Compare the users fingerprint from step 1. It must be identical. Verify
   `credential_uuid = users.proxy_uuid` for each imported row.

No command in this procedure writes `/usr/local/etc/xray/config.json`, reloads
Xray, or connects to production/Tky Proxy.

## Runtime environment contract

- Accounts: `BRIDGE_CREDENTIAL_TOKEN_PEPPER` and its existing
  `INTERNAL_SERVICE_TOKEN` must be supplied from deployment secret management.
- Bridge: `BRIDGE_ACCOUNTS_INTROSPECTION_URL`,
  `BRIDGE_ACCOUNTS_SERVICE_TOKEN` (the same value as Accounts
  `INTERNAL_SERVICE_TOKEN`), and `OPENCLAW_GATEWAY_SERVICE_TOKEN` are required.
- Caddy carries the user `Authorization` header unchanged; it holds no user
  credential. Bridge validates it with Accounts before task routing, then uses
  only `OPENCLAW_GATEWAY_SERVICE_TOKEN` for the OpenClaw connection.
- Compatibility owner: XWorkmate App. The App accepts the legacy response key
  `BRIDGE_AUTH_TOKEN` only when `bridgeCredential.token` is absent. Remove that
  parser branch after the UAT migration has verified every supported App build.
