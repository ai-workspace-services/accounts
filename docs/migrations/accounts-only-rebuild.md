# Independent database three-table rebuild (draft)

Owner: accounts owns schema, mapping and migratectl semantics. Playbooks owns
initialization, host execution, writer coordination, backups and acceptance.
Toolkit selects scope and validates immutable owner/run/release receipts.
No business data migration, deployment or production promotion has been executed.
This change is independent of schema-migration PRs #188 and #189.

## Contract

Use one reviewed migratectl binary for both ends. `export` and `import` gain
`--accounts-only` and `--dsn-env`; **`migrate` still only supports `--dsn` on main**.
Never claim controlled-migrate or migrate --dsn-env is merged.

```sh
# DSNs are injected by the authorized executor, never echoed or passed as argv.
migratectl export --accounts-only --dsn-env SOURCE_DSN --output protected-snapshot.yaml
migratectl import --accounts-only --dsn-env TARGET_DSN \
  --target-database accounts_rebuild_APPROVED_ID --file protected-snapshot.yaml --dry-run
# Real INSERT is separately authorized; dry-run does not prove INSERT constraints.
migratectl import --accounts-only --dsn-env TARGET_DSN \
  --target-database accounts_rebuild_APPROVED_ID --file protected-snapshot.yaml
```

The target must already be initialized from the specified release in a separately
selected `accounts_rebuild_<id>` database, not canonical `account` or the old DB.
Catalog/source provenance and all three empty arrays are mandatory. No email
filter, merge, timestamp strategy, skip-sessions, user rekey or partial allowlist.
Same observed database identity is rejected; endpoint aliases/proxies cannot be
proven distinct from connection names alone. Playbooks must independently bind
the approved source/target identity, isolated target selection and run evidence.
The catalog fingerprint is not proof of environment, release or source authority.

## Field mapping and dependencies

The accounts-only snapshot stores actual source column type, nullable/default and
generated status, and every row as JSON text. This preserves NULL versus absent
columns and bigint precision. Source catalogs are captured in one repeatable-read
read-only transaction with RLS visibility fail-closed. Types must match exactly;
no implicit cross-type conversions or fabricated semantic aliases are applied.

| Table | Accepted source → target fields | Target type / constraints |
| --- | --- | --- |
| users | uuid, proxy_uuid | UUID; user PK, proxy non-null; preserved, never regenerated |
| users | username, password, email | text; username/password non-null; email nullable; case-insensitive username/email unique indexes |
| users | role, level, groups, permissions, active | text / integer / jsonb / jsonb / boolean; non-null; preserve embedded permissions/state, no promotion or reset |
| users | created_at, updated_at | timestamptz non-null default now(); preserve timezone instant and microseconds |
| users | proxy_uuid_expires_at, subscription_valid_from/until, last_active_at, archived_at, mfa_secret_issued_at, mfa_confirmed_at, email_verified_at | nullable timestamptz; preserve if present |
| users | mfa_totp_secret, mfa_enabled | nullable text / non-null boolean default false; protected snapshot only, never receipt |
| users | email_verified | generated boolean; do not INSERT; source flag must agree with email_verified_at, otherwise reject rather than invent time |
| all three | version, origin_node | non-null bigint default 0 / text default local; preserve when present |
| identities | uuid, provider, external_id, user_uuid | UUID PK, non-null text/text, UUID FK users ON DELETE CASCADE; unique provider/external_id |
| sessions | uuid, token, expires_at, user_uuid | UUID PK, non-null text/timestamptz/UUID FK users ON DELETE CASCADE; token unique |
| identities/sessions | created_at, updated_at | non-null timestamptz default now(); absent source columns omitted, explicit NULL rejected |

Missing **semantic** user columns (including active, role, level, groups,
permissions and proxy_uuid) require mapping review and fail closed. Other missing
target columns use the reviewed target's default/NULL; existing values are never
silently dropped. Unknown `plan`, `revoked_at`, `identity_uuid` or other source
fields are rejected, not treated as irrelevant because business tables are absent.
Current sessions schema has no revocation or identity FK column; a source with
those fields needs an explicit compatibility decision before migration.
Expired and future sessions are retained exactly, not revived or extended.

## Conflict, rollback and retry

Users → identities → sessions; one serializable target transaction with all
three tables locked. Reject duplicate UUIDs, natural keys, email/username and
orphan FKs before writes. INSERT/constraint errors redact database detail and
rollback all rows. Never output tokens, passwords, MFA or DSNs in diagnostics.
Fresh target must have all three tables empty. Exact repeated import compares
all supplied non-generated fields and is a no-op; any changed or partial target
fails without repair, overwrite or merge. Missing timestamps generated by the
target are not presented as historical timestamps in acceptance.
Dry-run performs no INSERT/UPDATE/DELETE; it reads catalogs, casts and comparisons,
but cannot validate trigger/INSERT-only behavior. Real fixture INSERT is required.

Subscriptions/quota/billing history is not exported and no entitlements are
created. Existing embedded user rights remain mandatory. The selected source's
absence of new business tables must be evidenced separately; schema seeds may
contain static catalogs, not historical user subscriptions or charges.

## Local verification (2026-10-06)

After the final code edits, `go test ./...` and `git diff --check` passed.
`python3 scripts/tests/three_table_rebuild_test.py` passed on disposable
PostgreSQL 17.11 with a synthetic three-table source and real CLI export/INSERT/
replay. No external DSNs consumed; cluster stops and temporary data is removed.
Run with server binaries in PATH and `OPENCODE_TEST_TMP` pointing at an approved
temporary directory. Existing libpq-only initdb is insufficient.

Checks: UUID/email/hash/permissions/inactive state/FKs, microseconds/timezone,
missing timestamp defaults versus explicit NULL, empty identities/sessions,
rollback after a session constraint failure, exact rerun, conflicting and partial
target, same/non-independent target, unknown plan field and forbidden merge/rekey.
Remaining: actual fixture login, revoked/identity-linked source mapping,
full-release schema version/dirty checks, owner acceptance receipt, all cross-repo
contracts, and real original-user login. A synthetic hash comparison is not login.
No UAT, full upgrade/rollback/re-upgrade, real migration or PROD gate is proven.
