# Generation user reconciliation

`migratectl` owns only the read-only source identity audit and narrowly scoped
single-user backfill. It does not stop services, change traffic or DNS, validate
production routing, or move subscriptions, quota, usage, or finance records.
Those linked records cause a single-user apply to stop; they need a separately
reviewed reconciliation path. Playbooks owns host/service cutover gates, and DNS
changes remain with the IaC owner.

Use adjacent generation names (`prod-v2` → `prod-v3`, then `prod-v3` →
`prod-v4`). Supply DSNs via environment-variable names, never CLI arguments.
Use a read-only source role and a separately backed-up, isolated-restored target.

## Capture the pre-cutover UUID baseline

After the final source synchronization, with source writers isolated and source
and target user identity sets equal:

```sh
install -d -m 0700 /data/backups/web-saas/prod-v3/cutover
migratectl capture-user-baseline \
  --source-dsn-env ACCOUNTS_OLD_GENERATION_READONLY_DSN \
  --target-dsn-env ACCOUNTS_CURRENT_GENERATION_DSN \
  --source-generation prod-v2 \
  --target-generation prod-v3 \
  --environment prod \
  --schema-sha256 "$REVIEWED_ACCOUNTS_SCHEMA_SHA256" \
  --billing-schema-sha256 "$REVIEWED_BILLING_SCHEMA_SHA256" \
  --expected-target-version "$EXPECTED_TARGET_MIGRATION_VERSION" \
  > /data/backups/web-saas/prod-v3/cutover/source-user-baseline.json
chmod 0600 /data/backups/web-saas/prod-v3/cutover/source-user-baseline.json
```

The baseline contains a sorted set of one-way hashes of source UUIDs, counts,
the identity comparison, and a checksum. It does not contain account rows,
emails, password hashes, or raw user UUIDs. Protect it as operational evidence.

## Audit after traffic handoff

Keep the old generation read-only. This command compares its current UUID set
with the baseline and the current target. It deliberately does not infer late
registrations from `created_at`:

```sh
migratectl check-late-users \
  --source-dsn-env ACCOUNTS_OLD_GENERATION_READONLY_DSN \
  --target-dsn-env ACCOUNTS_CURRENT_GENERATION_DSN \
  --source-generation prod-v2 \
  --target-generation prod-v3 \
  --environment prod \
  --schema-sha256 "$REVIEWED_ACCOUNTS_SCHEMA_SHA256" \
  --billing-schema-sha256 "$REVIEWED_BILLING_SCHEMA_SHA256" \
  --expected-target-version "$EXPECTED_TARGET_MIGRATION_VERSION" \
  --baseline-file /data/backups/web-saas/prod-v3/cutover/source-user-baseline.json
```

Any source UUID absent from the baseline or any identity difference is reported
as a source-write-isolation anomaly. The command exits non-zero and emits only
aggregate counts; investigate why the old generation accepted a write before
attempting an individual backfill.

## Preview and apply one missing account

First set `ACCOUNTS_SELECTED_SOURCE_USER_UUID` to the exact canonical UUID
selected from an authorized source-side investigation. Run preview (the
default); review the privacy-safe receipt and preserve its `plan_sha256`:

```sh
migratectl sync-user \
  --source-dsn-env ACCOUNTS_OLD_GENERATION_READONLY_DSN \
  --target-dsn-env ACCOUNTS_CURRENT_GENERATION_DSN \
  --source-generation prod-v2 \
  --target-generation prod-v3 \
  --environment prod \
  --schema-sha256 "$REVIEWED_ACCOUNTS_SCHEMA_SHA256" \
  --billing-schema-sha256 "$REVIEWED_BILLING_SCHEMA_SHA256" \
  --expected-target-version "$EXPECTED_TARGET_MIGRATION_VERSION" \
  --source-user-id-env ACCOUNTS_SELECTED_SOURCE_USER_UUID
```

Only after the old source application writers are confirmed stopped, repeat the
UUID in a separate confirmation variable and pass the exact reviewed plan hash:

```sh
migratectl sync-user \
  --source-dsn-env ACCOUNTS_OLD_GENERATION_READONLY_DSN \
  --target-dsn-env ACCOUNTS_CURRENT_GENERATION_DSN \
  --source-generation prod-v2 \
  --target-generation prod-v3 \
  --environment prod \
  --schema-sha256 "$REVIEWED_ACCOUNTS_SCHEMA_SHA256" \
  --billing-schema-sha256 "$REVIEWED_BILLING_SCHEMA_SHA256" \
  --expected-target-version "$EXPECTED_TARGET_MIGRATION_VERSION" \
  --source-user-id-env ACCOUNTS_SELECTED_SOURCE_USER_UUID \
  --confirm-source-user-id-env ACCOUNTS_CONFIRM_SOURCE_USER_UUID \
  --source-writers-paused \
  --expected-plan-sha256 "$REVIEWED_PLAN_SHA256" \
  --apply
```

Apply inserts only a missing user row and its reviewed OAuth identity rows in
one target transaction. It never updates a target account, password, proxy UUID,
email, or account state. Existing matching accounts are an idempotent no-op.
ID/email/proxy/OAuth collisions, linked subscription/quota/usage/financial
records, schema drift, changed preview plan, or a source that is not provably
read-only stop the operation. Receipts contain a hash of the UUID, not the UUID
or account data.
