# XConnect referral native schema qualification — 2026-10-25

Qualified the local candidate in the isolated Accounts worktree based on
`3ef09c82f46152096bfc16edc9ba767f128a197f`, with the uncommitted referral campaign
migration and schema changes included. This is native artifact qualification,
not a deployment, production migration, full-business copy, writer switch or
runtime acceptance.

## Candidate

- PostgreSQL server: 17.11 (Debian 17.11-1.pgdg13+2); pg_dump: 17.11 (Homebrew).
- Disposable container: `xconnect-referral-native-20261025`, published only on
  `127.0.0.1:55440`. No host data volume, production DSN or application writer.
- Renderer fixture: `accounts_native_render`; initialization fixture: `account`.
- Migration version: `2026102501`.
- Business table count: 77 (previous manifest 74, three reviewed additions).
- Native SQL SHA-256:
  `e223518d156308c1cb2ea6919cfdccf1c0cacc03b55f9b41824ef29e4f632b13`.
- New migration source SHA-256:
  `30f67752741b94a62c9fc6a32123401e2ae71195fb285162f394b3f418ed622d`.

The manifest binds all current schema source hashes, including the changed
`sql/schema.sql`, qualifier and new migration. The business table list includes
`referral_campaign_revisions`, `referral_reward_campaign_snapshots` and
`referral_verified_invoices`, so a
full-business copy must supply their rows. Native SQL retains all checks,
foreign keys, append-only triggers and automatic reward campaign snapshot
trigger, but contains no business seed INSERT. Native initialization leaves
all three new tables empty; incremental migration defaults are not native business
facts.

## Commands and results

With environment variables pointing only to the disposable local fixtures:

```bash
NATIVE_SCHEMA_RENDER_DSN="$LOCAL_RENDER_DSN" go run ./scripts/native-schema-render
NATIVE_SCHEMA_RENDER_DSN="$LOCAL_RENDER_DSN" PG17_BIN=/opt/homebrew/opt/postgresql@17/bin \
  node scripts/qualify-native-schema.mjs
go test ./sql ./scripts/native-schema-render -count=1
MIGRATECTL_NATIVE_TEST_DSN="$LOCAL_NATIVE_TEST_DSN" \
  go test ./internal/migrate -run '^TestNativeInit' -count=1 -v
git diff --check -- sql scripts/qualify-native-schema.mjs
```

All commands passed. The qualifier emitted the table count, migration version
and SQL digest above. `TestNativeInitPostgres17` ran (not skipped) and verified
hidden-schema refusal, read-only dry-run, fresh initialization with zero
business rows, clean migration checkpoint, bounded forward upgrade without
historical replay, wrong digest rejection, repeated initialization refusal and
preservation of generated email verification and Proxy UUID.

The initialization test confirmed zero rows across all business tables. A
read-only catalog query after full-business qualification confirmed all four
campaign/verification triggers and all four new-table foreign keys. Static
source tests also reject top-level INSERT/UPDATE/DROP/GRANT/REVOKE and psql
metacommands in the native artifact. Function-body INSERT remains necessary
for transactional reward snapshots.

The existing qualifier's reviewed table scope and `sql/embed.go` exact table
count were updated for the three new tables. No CI workflow or environment was
changed. Fixture access was an authorized schema-management identity; this
exercise does not establish application or audit role isolation.

The disposable container was recreated for the final bridge-table and copy
qualification described below. No shared or pre-existing container was changed.

## Full-business and chunk transport compatibility

The reviewed full-business catalog now contains 78 tables: 77 native Accounts
tables and Billing's `cloud_vendor_costs`. It includes all three new referral
tables, their exact columns/types/keys and dependency order. The migration API
chunk allowlist includes these tables as well. `FullBusinessVersion` and fixture
checkpoints use `2026102501`; the older-source compatibility list remains
explicit rather than inferring tables from the source.

```bash
MIGRATECTL_FULL_BUSINESS_TEST_DSN="$LOCAL_FULL_COPY_DSN" \
  go test ./internal/migrate -run '^TestFullBusinessPostgres17$' -count=1 -v
MIGRATECTL_API_TEST_DSN="$LOCAL_API_CLUSTER_DSN" MIGRATECTL_API_TEST_REQUIRED=true \
  go test ./internal/migrate -run '^TestMigrationAPIPostgres17$' -count=1 -v
```

The full-business PostgreSQL 17 suite passed, including an older source with
optional tables absent, exact native copy with one real fictional row in every
new referral table, user references, sequence handling and late-trigger
rollback. The reward snapshot insert trigger remains enabled during copy. The
snapshot transport accepts a row already created by that trigger only when its
reward ID and campaign revision exactly match the source; a conflicting history
has an explicit rejection test. Final whole-table digests remain mandatory and
no constraint or trigger is disabled.

The final chunk API PostgreSQL 17 suite passed (32.45 seconds), including
restrictive source visibility, hash/content tamper, schema/function/baseline
drift, expiry, atomic late failure, concurrent/repeated commit, cloned receipt,
identity/OAuth collisions, dedicated target executor and interrupted HTTP
transfer/restart recovery. Both non-database package suites also passed again
after final artifact generation (`go test ./sql ./internal/migrate -count=1`).

The final disposable container was removed with its attached anonymous volume
after verification. Earlier anonymous fixture-volume cleanup was not asserted.
