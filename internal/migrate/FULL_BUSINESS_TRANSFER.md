# Reviewed full-business transfer

`migratectl copy-full-business` transports the 52 native Accounts tables and the
Billing-owned `cloud_vendor_costs` table. It is distinct from the old three-table
account export/import commands. The compiled Accounts SQL and Billing SQL SHA-256
are mandatory; the target must be the clean PostgreSQL 17 `account` database at
version `2026100701`, after native initialization and the bounded Billing upgrade.

## Execution contract

Playbooks must verify the original accepted CMDB/disk, exact prebuilt image,
independent production review and stopped target application/background writers
before invoking the CLI. Source and target DSNs are private environment variables;
command arguments never contain connection strings. Remote source connections
require TLS with no plaintext fallback. The source must use `readonly_release`:
no inheritance, membership, administrator/create/replication/bypass privileges,
table writes or sequence mutation. Every RLS table needs the exact reviewed
`release_initialization_readonly` permissive SELECT policy (`USING (true)`) and no
applicable restrictive policy. RLS remains enabled.

Defaults are read-only preview (`--dry-run=true`). Preview inspects scope,
columns, privileges and user matching; it does not stream the ledger or assert
equality. Actual copy requires `--dry-run=false` and an empty target across all
53 tables. It never deletes, truncates, resets, upserts into an existing dataset,
or disables constraints/triggers. A nonempty UAT database requires a separately
reviewed reconciliation operation; this baseline command does not modify it.

## Source-authoritative facts

Normalize email with lower/trim only for matching. Original PROD email bytes,
Proxy UUID, identity credentials, subscriptions, quota and ledger values remain
unchanged. Baseline copy retains source user UUIDs in the empty target.
`compare-full-business --dry-run=false` can compare a populated UAT dataset with
different user UUIDs by matching email; it remaps reviewed relational and semantic
user references before comparing. Matching user counts, email and PROD Proxy UUID
are mandatory. Historical non-user audit/service actors remain unchanged.

All existing source fields require reviewed names and exact types; unknown
public business relations, missing required fields or unreviewed conversions
fail closed. Eight new Accounts tables and the new Billing table may be absent
from a legacy source; their target datasets must be empty. Six absent legacy
`users.account_lifecycle_*` fields project to the approved native migration's
constant `active` state and NULL metadata. No legacy columns are created. A true
`email_verified` without its actual `email_verified_at` is rejected; no timestamp
is fabricated. Supabase-managed auth/storage schemas and operational migration
checkpoints are outside the business row scope.

## Bounded transport and verification

One repeatable-read, read-only MVCC source snapshot is held for the entire copy.
A NO SCROLL server cursor reads ordered primary-key batches of 1,000 rows,
including composite keys, without OFFSET scans or a query per row. Each row is
at most 1 MiB and each page at most 8 MiB. Source records are never written to a
plaintext snapshot file or emitted in an artifact/log. Only a batch and compact
64-byte key/row SHA-256 leaves per current table are kept in client memory.

A single target transaction holds exclusive business table locks and imports in
reviewed FK order, preserving FK/check/unique/insert guards and generated fields.
All target tables are verified after all inserts, preventing a later trigger
from silently changing a previously checked table. Equality uses the count plus
sorted primary-key/whole-row hashes for every target field. JSON keys and exact
numbers are canonicalized without converting integers or decimals through
float64; timestamps normalize to UTC. Equal row counts alone never pass.

Target identity/serial sequences advance above copied maxima without rewinding
existing high-water marks. No source `nextval` or source sequence mutation is
performed. PostgreSQL sequence advancement is nontransactional: a commit failure
may leave an empty target sequence advanced, which is safe for a retry. The tool
reports this sequence policy, not equality with unobservable source sequence
caches. Any batch/constraint/equality failure rolls back target business rows.

Receipts contain only schema identities, times, scope/counts, hashes and flags.
`database_cutover_approved` remains false even after copy/compare success. A
point-in-time snapshot cannot authorize a primary switch while source writers
continue. Playbooks and Edge still require final source writer freeze/catch-up,
fresh full-business equality, same Accounts/Billing primary, single-writer proof,
and production entry acceptance. No production copy or primary switch has been
performed by adding this tool.

## Qualification

The isolated PostgreSQL 17 CI job proves 44-table legacy source -> 53-table native
target, batches spanning 1,000 rows, composite PKs, >2^53 integer values, original
email/Proxy preservation, restrictive RLS refusal, transactional failure rollback,
source user immutability, sequence advancement, populated-target refusal,
email-based UUID equality, whole-field ledger mismatch rejection and unknown
source table refusal. Fixtures are synthetic and use only loopback databases.
These checks qualify the implementation; they are not production acceptance.
