# Account lifecycle schema upgrade (P0-1)

This migration is additive and does not rewrite account or subscription rows.
Do not apply it to UAT until P0-0 is complete and this PR has passed CI. This
procedure is for a separately approved UAT change window; it is not a release
or deployment authorization.

## Preflight and apply

1. Take and verify a transaction-consistent UAT database snapshot. Record the
   current migration version and a before fingerprint for the existing user
   and subscription attributes covered by issue #9.
2. Confirm the target DSN identifies UAT and that the runner uses this reviewed
   commit. Never use `sql/schema.sql`, `migratectl reset`, or a production data
   import for this upgrade.
3. From the repository root, apply only through the incremental runner:

   ```sh
   go run ./cmd/migratectl/main.go migrate --dsn "$UAT_ACCOUNTS_DSN" --dir sql/migrations
   go run ./cmd/migratectl/main.go version --dsn "$UAT_ACCOUNTS_DSN" --dir sql/migrations
   ```

   The expected current version is `2026092301` with `dirty=false`.
4. Verify the schema and retention guards:

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
   ```

   Confirm the six lifecycle columns, enabled row-level security, two user
   retention triggers and two audit immutability triggers. As the ordinary
   application role, a `SELECT` against `account_lifecycle_events` must return
   no rows (or be denied by grants). Compare the after fingerprint to the
   recorded before fingerprint; it must match exactly.

## Rollback and recovery

There is no down migration: deleting lifecycle audit history or disabling the
user retention guard is not an acceptable rollback. If the application release
must be reverted after a successful migration, roll back the application only;
the additive columns and table are compatible with the previous application.
If the database itself must be restored, use the verified pre-migration UAT
snapshot under the normal database recovery process and record the resulting
data-loss window. If migration execution fails, stop and inspect the migration
version and schema before any repair; do not run reset/schema initialization.

The repeatable PostgreSQL 17 upgrade/security checks live in
`sql/migrations/tests/account_lifecycle_upgrade_test.sql` and run in the PR CI
job. They require a fresh disposable database because the fixture creates the
legacy `public.users` table.
