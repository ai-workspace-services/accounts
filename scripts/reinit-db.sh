#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

echo ">>> 安全重跑业务 schema；已有业务表和记录会保留"
bash scripts/reset-public-schema.sh
bash scripts/init-db-core.sh
