# Latest native Accounts initialization

`accounts-native.sql` is the final PostgreSQL schema for a new empty Accounts
database: 52 business tables, current GORM columns, task-session control plane,
finance/lifecycle/MFA/recovery constraints, indexes, RLS and triggers. It contains
no users, RBAC/catalog seeds, subscriptions, quota or ledger rows. Full PROD copy
must supply the business data before a target writer is started.

This is an Accounts artifact. Billing's additional `cloud_vendor_costs` schema
belongs to the matching Billing release and must also be qualified by the
execution owner; it is not silently considered part of these 52 tables.

## Contract

The release image includes `/usr/local/bin/migratectl`, this SQL and its manifest.
The manifest binds the SQL hash, exact latest migration version, business table
names and source checksums. A new/changed incremental migration refuses native
initialization until a matching snapshot is qualified. Existing databases
continue through reviewed bounded incremental migrations; `init` always rejects
application relations/functions/types in any schema, even when public.users is
absent. It never resets/rebuilds a database or replays historical migrations.

Playbooks first validates the exact trusted CMDB target, independent persistent
disk, PostgreSQL bind and paused writers/CD controller. It then runs the fixed
prebuilt Accounts image with the target-only DSN supplied through an environment
variable. No production connection string is a workflow/command line argument.

```bash
migratectl native-schema
migratectl init --environment prod --dsn-env ACCOUNTS_TARGET_DSN \
  --schema-sha256 "$REVIEWED_NATIVE_SCHEMA_SHA256" --writers-paused
# Only after the owner confirms empty-target eligibility and paused writers:
migratectl init --environment prod --dsn-env ACCOUNTS_TARGET_DSN \
  --schema-sha256 "$REVIEWED_NATIVE_SCHEMA_SHA256" --writers-paused --dry-run=false
```

Dry-run is the default and writes no application schema. Actual initialization
holds the same advisory lock as bounded migration, enforces timeouts, executes
the compiled SQL in one transaction, verifies the exact table set and zero
business rows, and creates a clean `schema_migrations` version. Any failure rolls
back; successful repeat initialization is refused. Both receipts explicitly
set `database_cutover_approved=false`: they cannot substitute for full-business
equality, final catchup, single writer, gateway/CNAME switch or production entry
acceptance. Keep target Accounts/Billing and CD paused through the copy gate.

## Snapshot qualification

The initial snapshot was assembled in an isolated PostgreSQL 17 database from
`sql/schema.sql`, runtime RBAC/Billing DDL, current model/overlay AutoMigrate,
`tasksession.PostgresSchemaStatements` and all committed incremental migrations.
It is a final direct initialization SQL; the target does not execute the
historical compatibility scripts used when qualifying the development fixture.
The redundant `maintain_email_verified` trigger is excluded because the native
column is generated from `email_verified_at`, and cannot be assigned by a
writable compatibility trigger. No target business values are fabricated.

Source-drift and PostgreSQL 17 integration checks cover schema/hash/version,
empty dry-run, extension objects, hidden schema objects, fresh native init,
nonempty retry refusal and preservation of generated email verification and
Proxy UUID. These checks use fictional local/CI fixtures. They do not prove
PROD initialization, runtime startup or full data consistency.
