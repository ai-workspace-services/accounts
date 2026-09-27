# Account and billing history retention

Accounts retains user, subscription, payment, refund, and usage-ledger rows.
The admin DELETE route for a user performs an audited archive transition; it
does not delete the account row. Administrator and paid accounts remain
protected, and a repeated archive request returns a conflict.

The lifecycle migration installs PostgreSQL guards that reject DELETE and
TRUNCATE against public.users. The account-import path also protects local
profiles: merge skips existing users, username/email collisions fail before
writes, an existing-account UUID rekey fails before writes, and replace mode
requires an empty users table. These checks do not update subscription,
payment, refund, or usage-ledger history.

## Schema changes

Apply schema changes through forward-only, repeatable files in
sql/migrations/*.up.sql. Do not drop or rebuild existing tables. The
sql/schema.sql file is a non-destructive bootstrap baseline and may be
re-applied to create missing structures. migratectl reset is disabled.
migratectl clean --force only removes invalid indexes; it does not remove
tables or triggers.

Before an UAT release, verify the migration against a disposable PostgreSQL
database and capture an SQL scan for DROP TABLE, DROP SCHEMA, user DELETE, and
billing-history DELETE statements. The deliberate DELETE and TRUNCATE
statements in migration tests are rejection probes.
