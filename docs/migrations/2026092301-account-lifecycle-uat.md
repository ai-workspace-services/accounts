# Account lifecycle schema upgrade (P0-1)

This migration is additive and does not rewrite account or subscription rows.
Do not apply it until PR #170, P0-0, and the platform-ops controlled-migration
workflow change are all merged to their respective `main` branches and their
required checks pass. Start the cross-repository release only after all
participating PRs have merged. This runbook describes the UAT path; it does
not dispatch or run it.

## Read-only preflight

1. Take and verify a transaction-consistent UAT database snapshot. Record its
   identity and the pre-migration user/subscription row fingerprints without
   exporting row contents.
2. After all participating PRs merge, fetch Accounts `main` and calculate the
   digest from the merged migration file. The snapshot workflow tags this
   merged-main tree; its migration job independently verifies the digest
   against the file in that immutable Accounts tag before applying it.

   ```sh
   git fetch origin main --tags
   ACCOUNTS_MAIN_SHA="$(git rev-parse origin/main)"
   ACCOUNTS_SCHEMA_SHA256="$(git show "${ACCOUNTS_MAIN_SHA}:sql/migrations/2026092301_account_lifecycle_states.up.sql" | shasum -a 256 | awk '{print $1}')"
   printf 'Accounts main SHA: %s\nMigration SHA-256: %s\n' "$ACCOUNTS_MAIN_SHA" "$ACCOUNTS_SCHEMA_SHA256"
   ```

   Run this from the merged Accounts `main`, not the PR branch. Do not merge
   additional participating changes while the snapshot is resolving. Confirm
   the Accounts snapshot tag points to the recorded merged-main commit; the
   migration job also rejects a checksum mismatch.
3. Run the read-only probe through platform-ops `serverless-orchestrator.yml`
   on `main`:

   ```sh
   gh workflow run serverless-orchestrator.yml \
     --repo ai-workspace-infra/platform-ops-toolkit \
     --ref main \
     -f operation=plan \
     -f target_domains=web-saas \
     -f vault_env_path=uat \
     -f probe_accounts_schema=true \
     -f apply_accounts_schema_migration=false
   ```

   Before upgrade, require `migration_version=2026091401:false`,
   `lifecycle_columns=0/6`, and `lifecycle_events=false`. Stop if any value
   differs. The one-time baseline has already been adopted as version
   `2026091401`; do not adopt it again.

## Controlled UAT snapshot and migration

Use the merged-main `daily-main-snapshot.yaml` workflow for the full
cross-repository UAT snapshot. Choose a new immutable tag with the required
revision suffix, for example `uat-daily-build-YYYY.MM.DD-rN`, and provide the
SHA-256 calculated above:

```sh
gh workflow run daily-main-snapshot.yaml \
  --repo ai-workspace-infra/platform-ops-toolkit \
  --ref main \
  -f snapshot_tag=uat-daily-build-YYYY.MM.DD-rN \
  -f deploy_env=uat \
  -f enable_migration=false \
  -f adopt_accounts_baseline=false \
  -f apply_accounts_schema_migration=true \
  -f accounts_schema_expected_version=2026091401 \
  -f accounts_schema_target_version=2026092301 \
  -f accounts_schema_sha256="$ACCOUNTS_SCHEMA_SHA256"
```

Replace the example tag with a new unused UAT daily tag. Leave
`snapshot_source_ref` and `repositories` unset: the workflow requires an
unfiltered snapshot from merged `main`. `enable_migration=false` prevents the
PROD-to-UAT data merge. `adopt_accounts_baseline=false` avoids repeating the
already completed baseline adoption. The workflow dispatches the serverless
deployment with `operation=deploy`; its controlled migration job requires the
UAT schema to be exactly `2026091401:false`, checks that `2026092301` is the
only pending migration in the Accounts snapshot tag, verifies the file's
SHA-256, and confirms `2026092301:false` after `migratectl` applies it. Do not
run `migratectl migrate` manually, because that can apply all pending
migrations outside the snapshot gate.

## After probe

After the workflow completes, run the same read-only
`serverless-orchestrator.yml` probe from the preflight section. Require
`migration_version=2026092301:false`, `lifecycle_columns=6/6`, and
`lifecycle_events=true`. Also verify these database guards:

```sql
SELECT column_name
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'users'
  AND column_name LIKE 'account_lifecycle_%'
ORDER BY column_name;

SELECT relrowsecurity
FROM pg_class
WHERE oid = 'public.account_lifecycle_events'::regclass;

SELECT tgname
FROM pg_trigger
WHERE tgrelid IN ('public.users'::regclass, 'public.account_lifecycle_events'::regclass)
  AND NOT tgisinternal
ORDER BY tgname;

SELECT conname, confdeltype
FROM pg_constraint
WHERE conrelid = 'public.account_lifecycle_events'::regclass
  AND confrelid = 'public.users'::regclass
  AND contype = 'f';
```

Confirm the six lifecycle columns, `relrowsecurity=true`, the user retention
triggers (`users_no_delete_trg`, `users_no_truncate_trg`), audit immutability
triggers (`account_lifecycle_events_immutable_trg`,
`account_lifecycle_events_no_truncate_trg`), and audit foreign-key
`confdeltype='r'` (`ON DELETE RESTRICT`). As the ordinary application role,
with `SELECT` privilege (for example, Supabase `authenticated`),
`SELECT count(*) FROM public.account_lifecycle_events` must return `0`;
permission denial is also acceptable. Compare the after user/subscription
fingerprints with the before values; they must match exactly.

## Rollback and recovery

There is no down migration: deleting lifecycle audit history or disabling the
user retention guard is not an acceptable rollback. The schema is additive,
so if a Cloud Run deployment fails after migration, roll back the application
to its previous immutable tag and leave the schema in place. If the database
itself must be restored, restore the verified pre-migration UAT snapshot under
the normal database recovery process and record the resulting data-loss
window. If the migration job fails or reports a dirty/unexpected version,
stop; do not rerun the snapshot, force a migration version, or use
reset/schema initialization. Inspect migration state and recover from the
checkpoint through the controlled database recovery process.

The repeatable PostgreSQL 17 upgrade/security checks live in
`sql/migrations/tests/account_lifecycle_upgrade_test.sql` and run in PR CI.
They require a fresh disposable database because the fixture creates the
legacy `public.users` table.
