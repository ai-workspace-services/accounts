# Online business append with migratectl

`migratectl reconcile-business` supplements a users-only migration into an
already populated target. Use a checksum-verified prebuilt release asset; never
build on the target. This is an explicit persistent-business scope, not a
replacement for the full-business empty-target recovery command.

The reviewed scope contains billing plans, account billing profiles, quota and
policy snapshots, subscriptions and their records/lifecycle state, traffic
checkpoints/minute buckets, billing ledger, invoices/payments/refunds, and
financial operation records/events. It never copies user rows, sessions,
credentials, running node observations or service configuration.

The source is a read-only repeatable-read snapshot using the dedicated reader
role. Source users must match the unique normalized target email, password hash
and proxy UUID; related rows map to the existing target account UUID. An explicit
`--ignore-default-sandbox-proxy-drift` permits only the product's default Sandbox
identity to retain its different target proxy UUID. It does not waive email or
password matching, allow another user's proxy mismatch, or change credentials.

Preview is the default and emits only aggregate counts and fingerprints. Apply
requires its exact `--expected-plan-sha256`, consecutive PROD generations, the
clean target version and both compiled schema checksums. Apply acquires target
table and migration locks before reading target data, rebuilds the plan, inserts
missing rows in FK order, verifies complete row fingerprints and protected
non-scope tables, and releases locks on commit or rollback. Applications remain
online; writers may wait briefly while the bounded transaction runs. A changed
source business data, selected target rows or validated identity mapping requires a new preview. Unrelated runtime heartbeats do not invalidate a reviewed business plan. Schema fingerprints exclude PostgreSQL planner/vacuum statistics, while retaining structural definitions and access policies. No `--writers-paused` assertion
is needed on this append-only command.

A matching key with any different field is a conflict, including different
money, quota, policy or subscription status. Existing rows are never overwritten,
deleted, reset or silently replaced; target-only rows remain. Copying a missing
quota snapshot is refused if independent target-only accounting facts exist.
The owner must review these conflicts rather than disabling guards.

Native initialization does not manufacture a controlled-upgrade history table.
The schema fingerprint includes its explicit presence/absence; migration history
is locked and fingerprinted when present. No history table is created by append.

After all row/schema checks, the target finance event identity sequence is only
advanced when below copied IDs. PostgreSQL sequence allocation is nontransactional:
a failed final commit can leave an allocation gap, never a rewritten financial
fact or a sequence decrement. Replays do not advance an already sufficient value.

Example (DSN values remain in protected process environment, not arguments):

```sh
migratectl reconcile-business \
  --source-dsn-env SOURCE_MIGRATION_DSN --target-dsn-env TARGET_MIGRATION_DSN \
  --source-generation prod-v4 --target-generation prod-v5 \
  --expected-target-version <reviewed-version> \
  --schema-sha256 <compiled-native-checksum> \
  --billing-schema-sha256 <compiled-billing-contract-checksum>
# Repeat the identical command with --apply --expected-plan-sha256 <preview-sha>.
```

Before application, retain a verified target rollback snapshot and source capture
identity. Keep target financial background execution disabled while the source
still owns in-flight financial jobs; copying an operation record never authorizes
a second provider operation or a source writer handoff. The operator separately
verifies this runtime policy. Exporters continue through Vector, and collection
acceptance requires actual fresh checkpoints, positive byte deltas and the
Accounts/browser boundary. Migration equality is not traffic acceptance.
