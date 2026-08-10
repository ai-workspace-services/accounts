#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

SUPERADMIN_EMAIL="${SUPERADMIN_EMAIL:-admin@svc.plus}"
SUPERADMIN_USERNAME="${SUPERADMIN_USERNAME:-Admin}"

if [ -z "${SUPERADMIN_PASSWORD:-}" ] || [ "${SUPERADMIN_PASSWORD}" = "ChangeMe" ]; then
  if command -v openssl >/dev/null; then
    SUPERADMIN_PASSWORD="$(openssl rand -base64 12 | tr -d '\n' | tr '/+' 'Aa' | cut -c1-10)"
  else
    SUPERADMIN_PASSWORD="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 10)"
  fi
  echo "⚠️ 未指定 SUPERADMIN_PASSWORD，已生成随机初始密码: ${SUPERADMIN_PASSWORD}"
fi

if psql "${DB_URL}" -Atc "SELECT 1 FROM users WHERE username='${SUPERADMIN_USERNAME}' OR email='${SUPERADMIN_EMAIL}' LIMIT 1" | grep -qx '1'; then
  if [ -z "${SUPERADMIN_CURRENT_PASSWORD:-}" ]; then
    echo "⚠️ 超级管理员已存在，未提供 SUPERADMIN_CURRENT_PASSWORD，跳过更新"
    exit 0
  fi
  go run ./cmd/createadmin/main.go \
    --driver postgres \
    --dsn "${DB_URL}" \
    --username "${SUPERADMIN_USERNAME}" \
    --password "${SUPERADMIN_PASSWORD}" \
    --email "${SUPERADMIN_EMAIL}" \
    --current-password "${SUPERADMIN_CURRENT_PASSWORD}"
  echo "✅ 登录信息（仅本次输出）"
  echo "   网址：https://console.svc.plus/login"
  echo "   邮箱：${SUPERADMIN_EMAIL}"
  echo "   密码：${SUPERADMIN_PASSWORD}"
  exit 0
fi

go run ./cmd/createadmin/main.go \
  --driver postgres \
  --dsn "${DB_URL}" \
  --username "${SUPERADMIN_USERNAME}" \
  --password "${SUPERADMIN_PASSWORD}" \
  --email "${SUPERADMIN_EMAIL}"

echo "✅ 登录信息（仅本次输出）"
echo "   网址：https://console.svc.plus/login"
echo "   邮箱：${SUPERADMIN_EMAIL}"
echo "   密码：${SUPERADMIN_PASSWORD}"
