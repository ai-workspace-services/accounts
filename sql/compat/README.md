# Accounts target migration-scope compatibility

`target-scope-upgrade.sql` is a **target-only, review-stage** additive SQL
artifact for an existing PostgreSQL 17 Accounts database whose exact observed
starting shape has 21 of the migration API's 42 tables and no
`public.schema_migrations`. It is not a general-purpose schema upgrade or a
replacement for the empty-database native initializer.

The 21 new table definitions, indexes, RLS settings, triggers and five trigger
functions are derived from `sql/init/accounts-native.sql`, SHA-256
`80ca865a9b93f1e63ca4c4c76e7a08e69458245ec63f1bba5667e28c4409a619`.
The artifact also makes `users.email` nullable and adds three validated foreign
keys to existing tables. It creates no application checkpoint and no business
rows. It does not install migration-role policies, grants, trigger guards,
sequence helpers or the listener.

The SQL is one transaction with explicit lock and statement timeouts. It
requires the caller to bind the independently verified PostgreSQL system
identifier and target database OID in session settings
`accounts_migration_guard.system_identifier` and
`accounts_migration_guard.database_oid`. It rejects a missing or mismatched
identity, non-17 server, existing application checkpoint, absent expected
tables, already-created target objects/functions, or a different email
nullability. PostgreSQL rolls the transaction back on a failed constraint or
DDL. A repeat invocation **refuses** an already-modified target; it is not an
automatic success receipt. Inspect target state and the operation's external
receipt before retrying.

Before any live execution, Playbooks must independently bind the exact target,
SQL SHA-256 and current schema, verify an encrypted backup with an isolated
populated restore, control target writers, and provide a recovery procedure.
The SQL itself does not verify that backup. Service-owner review must cover
the effect of the new RLS tables, grants and foreign keys. Existing target
triggers, constraints, indexes and non-migration tables are not reconciled to
the full native schema. In particular, validation of 42 column/PK/FK shapes
does not prove trigger equivalence: the observed target differs from the native
artifact by nine triggers on existing lifecycle, RBAC and user tables. Their
business behavior requires review before release. The current API still requires a clean
reviewed schema checkpoint; this file intentionally does not fabricate one.

The observed V6 gap has three distinct effects: two lifecycle triggers enforce
append-only audit events; two user triggers reject hard deletion or truncation;
and five triggers update timestamps or user versions on ordinary updates. The
catch-up classifier treats lifecycle events as immutable facts and checks full
row fingerprints, but that protects its own transfer path only; it does not
provide the missing database enforcement for other V6 writers. Conversely,
installing the five update triggers changes ordinary business write semantics
and migration-row fingerprints; do not silently add them in this compatibility
operation. The fixture explicitly models all nine as absent and asserts they
remain absent after this SQL. This is a documented compatibility limitation,
not an assertion that the target is equivalent to the native schema.

`TestTargetScopeUpgradePostgres` builds and destroys only a newly named
synthetic database in an explicitly supplied loopback PostgreSQL 17 fixture. It
tests target-identity rejection, data preservation, the 42-table catalog,
absence of a forged checkpoint and refusal of an unverified retry. A real
backup was also exercised separately in an isolated review database; that
evidence belongs in the operation record, not in this SQL's guarantees.
