# Controlled Accounts migration runbook

`migratectl controlled-migrate` applies exactly one reviewed, checked-in
`.up.sql` migration. It is intended for a controlled UAT upgrade where the
unbounded legacy `migrate` command is too broad. Existing `migrate` behavior is
unchanged.

The executable selects the SQL by `--target-version` from files embedded at
build time. It does not accept a DSN flag, SQL text, a migration path, or a
force option. The DSN is read from `ACCOUNTS_MIGRATION_DSN` only. The running
Docker image contains `/usr/local/bin/migratectl` and `/app/sql/migrations`;
the binary embeds the same checked-in migrations so it can also run from a
Cloud Run container command without relying on a source checkout.

## Review and invocation

Before execution, review the exact `.up.sql` diff and independently calculate
its SHA-256 from the same source revision that produced the Accounts image.
Confirm the database's current `schema_migrations.version` is the expected
version. Then pass all three reviewed values:

```sh
export ACCOUNTS_MIGRATION_DSN="$UAT_ACCOUNTS_DSN"
migratectl controlled-migrate \
  --expected-version 2026092703 \
  --target-version 2026092801 \
  --sha256 <64-character-reviewed-sha256> \
  --lock-timeout 10s \
  --statement-timeout 2m
```

Replace versions and checksum with the values for the specific reviewed
migration. Do not copy the illustrative values without verifying that the
target is the immediate next migration in the image. Use the same image
reference and full commit identity recorded by the release workflow; this
command does not build or select an image.

The runner checks the expected starting version, refuses dirty migration
state, and permits only one forward step. SQL, `schema_migrations` version,
and checksum history commit in one transaction. It takes the same PostgreSQL
advisory lock ID as golang-migrate for the current database, schema, and
`schema_migrations` table. Lock acquisition and each SQL statement have
bounded timeouts. An exact retry after a successful commit is a no-op only if
the persisted source version and SQL checksum match. Legacy-applied versions
without controlled checksum history are not treated as a controlled replay.

Checked-in files may have a top-level `BEGIN; ... COMMIT;` wrapper. The
controlled runner lexes SQL strings, comments, identifiers, and dollar-quoted
bodies, removes only a complete first-statement `BEGIN` / final-statement
`COMMIT` pair, then executes the remaining statements inside its own
transaction. Any incomplete, nested, or other transaction-control statement
is rejected. Do not add transaction control to procedural SQL bodies as
standalone top-level statements.

Failures return generic messages without driver details or the DSN. A failed
statement rolls back SQL, version, and checksum-history changes together. Do
not use `--force` or alter `schema_migrations` manually to bypass a refusal;
diagnose the exact version and checksum state first.

## Verification after execution

Record the Accounts image reference/full source commit, expected and target
versions, reviewed SHA-256, command outcome, and resulting database version.
Then run the separate UAT application smoke and release validation. A successful
local CLI test or migration command is not by itself evidence that the UAT
deployment or application is healthy.
