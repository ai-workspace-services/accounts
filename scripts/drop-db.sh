#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

echo "drop-db is disabled: account, subscription, payment, refund, and usage history must be retained." >&2
echo "Use a separate disposable database for local development." >&2
exit 1
