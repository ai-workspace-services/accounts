#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

if command -v air >/dev/null; then
  air
  exit 0
fi

echo "❌ 未检测到 air (热重载工具)，请先安装: go install github.com/air-verse/air@latest"
exit 0
