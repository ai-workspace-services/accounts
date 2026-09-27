#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

echo "db-reset is disabled: account, subscription, payment, refund, and usage history must be retained." >&2
echo "Use forward-only migrations for schema upgrades." >&2
exit 1
