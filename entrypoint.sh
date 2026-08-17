#!/usr/bin/env bash
set -euo pipefail

# -----------------------------------------------------------------------------
# Database tunnel bootstrap
# -----------------------------------------------------------------------------

USE_STUNNEL=0
STUNNEL_CONF="/etc/stunnel/stunnel.conf"

# Cloud Run receives the Supabase Session pooler as a full connection URI.
# That endpoint already terminates TLS, so it must not be routed through the
# legacy self-hosted PostgreSQL stunnel client even when inherited DB_TLS_*
# variables are present. The stunnel client remains exclusively for the
# self-hosted PostgreSQL -> remote stunnel-server topology.
SUPABASE_RUNTIME_URI="${SUPABASE_CONNECT_URI:-${SUPABASE_CONNECT_URL:-}}"

if [ -z "${DB_USER:-}" ] && [ -n "${POSTGRES_USER:-}" ]; then
  export DB_USER="${POSTGRES_USER}"
fi

if [ -z "${DB_PASSWORD:-}" ] && [ -n "${POSTGRES_PASSWORD:-}" ]; then
  export DB_PASSWORD="${POSTGRES_PASSWORD}"
fi

if [ -z "${POSTGRES_USER:-}" ] && [ -n "${DB_USER:-}" ]; then
  export POSTGRES_USER="${DB_USER}"
fi

if [ -z "${POSTGRES_PASSWORD:-}" ] && [ -n "${DB_PASSWORD:-}" ]; then
  export POSTGRES_PASSWORD="${DB_PASSWORD}"
fi

if [ -z "${DB_NAME:-}" ] && [ -n "${POSTGRES_DB:-}" ]; then
  export DB_NAME="${POSTGRES_DB}"
fi

if [ -z "${SUPABASE_RUNTIME_URI}" ] && [ -n "${DB_TLS_HOST:-}" ] && [ -n "${DB_TLS_PORT:-}" ]; then
  USE_STUNNEL=1
  export DB_HOST="${DB_HOST:-127.0.0.1}"
  export DB_PORT="${DB_PORT:-15432}"

  mkdir -p /etc/stunnel /var/run/stunnel

  if [ -n "${DB_CA:-}" ]; then
    printf '%s\n' "${DB_CA}" > /etc/stunnel/ca.pem
  fi

  cat > "${STUNNEL_CONF}" <<EOF
foreground = no
pid = /tmp/stunnel.pid
socket = l:TCP_NODELAY=1
socket = r:TCP_NODELAY=1

[postgres-client]
client = yes
accept = ${DB_HOST}:${DB_PORT}
connect = ${DB_TLS_HOST}:${DB_TLS_PORT}
verify = 2
EOF

  if [ -f "/etc/stunnel/ca.pem" ]; then
    echo "CAfile = /etc/stunnel/ca.pem" >> "${STUNNEL_CONF}"
  elif [ -f "/etc/ssl/certs/ca-certificates.crt" ]; then
    echo "CAfile = /etc/ssl/certs/ca-certificates.crt" >> "${STUNNEL_CONF}"
  fi

  if [ -n "${DB_TLS_SERVER_NAME:-}" ]; then
    echo "checkHost = ${DB_TLS_SERVER_NAME}" >> "${STUNNEL_CONF}"
  elif [ -n "${DB_TLS_HOST:-}" ]; then
    echo "checkHost = ${DB_TLS_HOST}" >> "${STUNNEL_CONF}"
  fi
fi

CONFIG_FILE="${CONFIG_PATH:-/etc/xcontrol/account.yaml}"
CONFIG_TEMPLATE="${CONFIG_TEMPLATE:-/app/config/account.yaml}"
mkdir -p "$(dirname "${CONFIG_FILE}")"

if [ ! -f "${CONFIG_FILE}" ]; then
  if [ -f "${CONFIG_TEMPLATE}" ]; then
    envsubst < "${CONFIG_TEMPLATE}" > "${CONFIG_FILE}"
  else
    echo "missing config template: ${CONFIG_TEMPLATE}" >&2
    exit 1
  fi
fi

if [ -n "${PORT:-}" ]; then
  tmp_cfg=$(mktemp)
  awk -v port="$PORT" '
    /^server:/ {print; in_server=1; addr_written=0; next}
    in_server && /^  addr:/ {print "  addr: \":" port "\""; addr_written=1; next}
    in_server && /^ [^ ]/ {in_server=0}
    {print}
    END {
      if (port != "" && in_server == 0 && addr_written == 0) {
        print "server:";
        print "  addr: \":" port "\"";
      }
    }
  ' "${CONFIG_FILE}" > "${tmp_cfg}"
  CONFIG_FILE="${tmp_cfg}"
fi

DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-15432}"

if [ "${USE_STUNNEL}" -eq 1 ]; then
  if ! command -v stunnel >/dev/null 2>&1; then
    echo "stunnel is required but not installed" >&2
    exit 1
  fi

  stunnel "${STUNNEL_CONF}"
fi

if [ "${USE_STUNNEL}" -eq 1 ] && command -v nc >/dev/null 2>&1; then
  wait_seconds="${STUNNEL_WAIT_SECONDS:-30}"
  i=0
  while ! nc -z "${DB_HOST}" "${DB_PORT}" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "${i}" -ge "${wait_seconds}" ]; then
      echo "stunnel not ready after ${wait_seconds}s on ${DB_HOST}:${DB_PORT}" >&2
      break
    fi
    sleep 1
  done
fi

exec /usr/local/bin/account --config "${CONFIG_FILE}" "$@"
