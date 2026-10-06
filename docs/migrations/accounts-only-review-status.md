# Review status: accounts-only rebuild

Stage 1 implementation and local INSERT/replay/rollback verification complete.
P0 overall is **not complete**: Playbooks, bound accounts-only acceptance,
fixture login and release migration-state checks remain required.

- Baseline: c2c343fe2c91bb31e7f7f9b4fa60512a66e2b4c9; independent of #188/#189.
- Branch: fix/three-table-rebuild-20261006; draft review only.
- Owner: accounts; caller: Playbooks (not yet switched).
- Side effects: authorized isolated target INSERT only; no cloud/host execution.
- Evidence: synthetic PostgreSQL 17.11 test, `go test ./...`, `git diff --check` passed.
- No raw logs, snapshots, credentials or business records are part of this PR.
- Not run: real original-user login/data, UAT, deploy/cutover, full upgrade/rollback,
  production promotion. No IAM/Vault/state changes.
- Dependency order: reviewed accounts binary → Playbooks owner → reviewed immutable
  owner SHA/allowlist → Toolkit caller → authorized UAT → legacy deletion last.
