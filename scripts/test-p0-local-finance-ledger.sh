#!/usr/bin/env bash
# P0-F regression gate. By default it only exercises in-process Go tests.
# The PostgreSQL migration fixture is run by CI; set P0_POSTGRES_URL to run it
# against an explicitly supplied disposable database.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/_common.sh"

go test -count=1 ./internal/store
go test -race -count=1 ./internal/store

if [[ -n "${P0_POSTGRES_URL:-}" ]]; then
  psql "$P0_POSTGRES_URL" -v ON_ERROR_STOP=1 \
    -f sql/migrations/tests/local_finance_ledger_upgrade_test.sql
fi
