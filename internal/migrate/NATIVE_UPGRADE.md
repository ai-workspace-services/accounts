# Bounded forward migration after direct native initialization

`migratectl init` establishes the latest clean schema version without replaying
historical migration files. The next bounded `migratectl migrate` step must
therefore work with only the checksum-qualified next SQL file on disk.

The bounded runner retains its exact expected/target versions, one pending
migration, SHA-256, database advisory lock, dirty-state refusal and timeout checks.
It now supplies golang-migrate with a read-only in-memory source containing the
already-applied version as metadata and exactly one executable next SQL body.
The checkpoint has no SQL. `First` and `Next` only select the reviewed target;
there are no previous migrations or down bodies. The validated SQL bytes are
retained in memory so a later file edit cannot change the executed body.

The development `Up`/file source and historical APIs are unchanged. No schema
seed, compatibility column, historical replay or database reset is introduced.
PostgreSQL 17 qualification initializes the compiled native SQL, applies a single
new fixture table without old-version files, verifies the clean target, repeated
upgrade idempotence and wrong-digest refusal, and retains user/Proxy UUID checks.
Deploy still consumes a prebuilt full-SHA Accounts image with an exact digest.
