#!/usr/bin/env bash
# 幂等地把 scripts/stripe-catalog.yaml 同步到 Stripe：Product、Price、
# Webhook Endpoint 全部走 API 创建，不需要在 Dashboard 里点。
#
# 幂等策略：
#   - Product 用目录里的 key 作为 Stripe 自定义 id；已存在就更新
#     name/description，不存在就创建。
#   - Price 在 Stripe 里创建后金额不可变，所以用 lookup_key 做存在性判断：
#     已存在就跳过创建、只核对金额是否与目录一致(不一致只警告，绝不
#     自动改价——那必须是新价格/新 key)；不存在才创建。
#   - Webhook Endpoint 按 url 匹配；已存在就同步 enabled_events，不存在
#     才创建。签名密钥(secret)只在创建那一刻由 Stripe 返回一次，本脚本
#     原样打印，不写入任何文件。
#
# 换 Stripe 账号(例如现在的 sandbox 切到 Stripe US 生产账号)= 换一个
# STRIPE_SECRET_KEY 重跑本脚本，不需要任何手工 Dashboard 操作。
#
# 用法:
#   STRIPE_SECRET_KEY=sk_test_... \
#     scripts/stripe-sync-catalog.sh --env uat --domain-base onwalk.net
#
#   加 --dry-run 只打印将要做什么，不实际调用会产生副作用的 Stripe API。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CATALOG_FILE="${SCRIPT_DIR}/stripe-catalog.yaml"
STRIPE_API_BASE="${STRIPE_API_BASE:-https://api.stripe.com/v1}"
DRY_RUN=false
ENV_NAME=""
DOMAIN_BASE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env) ENV_NAME="$2"; shift 2 ;;
    --domain-base) DOMAIN_BASE="$2"; shift 2 ;;
    --catalog) CATALOG_FILE="$2"; shift 2 ;;
    --dry-run) DRY_RUN=true; shift ;;
    *) echo "::error::unknown argument: $1" >&2; exit 1 ;;
  esac
done

: "${STRIPE_SECRET_KEY:?STRIPE_SECRET_KEY is required (sk_test_... for sandbox, sk_live_... for a live account)}"
[[ -n "${ENV_NAME}" ]] || { echo "::error::--env is required (e.g. uat, prod)" >&2; exit 1; }
[[ -n "${DOMAIN_BASE}" ]] || { echo "::error::--domain-base is required (e.g. onwalk.net, svc.plus)" >&2; exit 1; }
[[ -f "${CATALOG_FILE}" ]] || { echo "::error::catalog file not found: ${CATALOG_FILE}" >&2; exit 1; }

command -v jq >/dev/null || { echo "::error::jq is required" >&2; exit 1; }
python3 -c "import yaml" 2>/dev/null || { echo "::error::python3 + pyyaml is required (pip install pyyaml)" >&2; exit 1; }

catalog_json="$(python3 -c "import sys, yaml, json; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)" "${CATALOG_FILE}")"

auth=(-u "${STRIPE_SECRET_KEY}:")

stripe_get() {
  curl -sS "${auth[@]}" "${STRIPE_API_BASE}$1"
}
stripe_post() {
  local path="$1"; shift
  curl -sS "${auth[@]}" -X POST "${STRIPE_API_BASE}${path}" "$@"
}
stripe_patch() {
  local path="$1"; shift
  curl -sS "${auth[@]}" -X POST "${STRIPE_API_BASE}${path}" "$@"
}

echo "== Stripe catalog sync =="
echo "env=${ENV_NAME} domain_base=${DOMAIN_BASE} dry_run=${DRY_RUN} catalog=${CATALOG_FILE}"
echo

# --- Products + Prices --------------------------------------------------
price_summary=()

product_count=$(jq '.products | length' <<<"${catalog_json}")
for ((pi = 0; pi < product_count; pi++)); do
  product="$(jq ".products[$pi]" <<<"${catalog_json}")"
  pkey="$(jq -r '.key' <<<"${product}")"
  pname="$(jq -r '.name' <<<"${product}")"
  pdesc="$(jq -r '.description // ""' <<<"${product}")"

  existing="$(stripe_get "/products/${pkey}")"
  if jq -e '.id' <<<"${existing}" >/dev/null 2>&1; then
    echo "product ${pkey}: exists"
    if [[ "${DRY_RUN}" == "false" ]]; then
      stripe_patch "/products/${pkey}" \
        --data-urlencode "name=${pname}" \
        --data-urlencode "description=${pdesc}" >/dev/null
    fi
  else
    echo "product ${pkey}: creating"
    if [[ "${DRY_RUN}" == "false" ]]; then
      created="$(stripe_post "/products" \
        --data-urlencode "id=${pkey}" \
        --data-urlencode "name=${pname}" \
        --data-urlencode "description=${pdesc}")"
      jq -e '.id' <<<"${created}" >/dev/null || {
        echo "::error::failed to create product ${pkey}: $(jq -c . <<<"${created}")" >&2
        exit 1
      }
    fi
  fi

  price_count=$(jq '.prices | length' <<<"${product}")
  for ((qi = 0; qi < price_count; qi++)); do
    price="$(jq ".prices[$qi]" <<<"${product}")"
    qkey="$(jq -r '.key' <<<"${price}")"
    plan_id="$(jq -r '.plan_id' <<<"${price}")"
    amount="$(jq -r '.unit_amount' <<<"${price}")"
    currency="$(jq -r '.currency' <<<"${price}")"
    interval="$(jq -r '.recurring_interval // ""' <<<"${price}")"

    lookup="$(stripe_get "/prices?lookup_keys[]=${qkey}&active=true")"
    found_id="$(jq -r '.data[0].id // ""' <<<"${lookup}")"

    if [[ -n "${found_id}" ]]; then
      existing_amount="$(jq -r '.data[0].unit_amount' <<<"${lookup}")"
      if [[ "${existing_amount}" != "${amount}" ]]; then
        echo "::warning::price ${qkey} (${found_id}) is ${existing_amount}, catalog says ${amount} — Stripe prices are immutable, bump the lookup_key (e.g. ${qkey}-v2) to change the amount, do not edit in place"
      fi
      echo "price ${qkey}: exists -> ${found_id}"
      price_id="${found_id}"
    else
      echo "price ${qkey}: creating (${amount} ${currency}$([[ -n "${interval}" ]] && echo "/${interval}"))"
      if [[ "${DRY_RUN}" == "false" ]]; then
        args=(--data-urlencode "product=${pkey}"
              --data-urlencode "unit_amount=${amount}"
              --data-urlencode "currency=${currency}"
              --data-urlencode "lookup_key=${qkey}")
        [[ -n "${interval}" ]] && args+=(--data-urlencode "recurring[interval]=${interval}")
        created="$(stripe_post "/prices" "${args[@]}")"
        price_id="$(jq -r '.id // ""' <<<"${created}")"
        [[ -n "${price_id}" ]] || {
          echo "::error::failed to create price ${qkey}: $(jq -c . <<<"${created}")" >&2
          exit 1
        }
      else
        price_id="(dry-run, not created)"
      fi
    fi
    price_summary+=("${plan_id}|${price_id}")
  done
done

# --- Webhook endpoint -----------------------------------------------------
webhook_path="$(jq -r '.webhook.path' <<<"${catalog_json}")"
webhook_url="https://accounts-${ENV_NAME}.${DOMAIN_BASE}${webhook_path}"
# prod 沿用既有域名约定：accounts.<domain_base>，不带 env 前缀。
[[ "${ENV_NAME}" == "prod" ]] && webhook_url="https://accounts.${DOMAIN_BASE}${webhook_path}"

mapfile -t events < <(jq -r '.webhook.events[]' <<<"${catalog_json}")

echo
echo "webhook url: ${webhook_url}"

existing_endpoints="$(stripe_get "/webhook_endpoints?limit=100")"
endpoint_id="$(jq -r --arg url "${webhook_url}" '.data[] | select(.url == $url) | .id' <<<"${existing_endpoints}" | head -1)"

if [[ -n "${endpoint_id}" ]]; then
  echo "webhook endpoint: exists -> ${endpoint_id} (signing secret unchanged, not re-printed — Stripe only returns it once, at creation)"
  if [[ "${DRY_RUN}" == "false" ]]; then
    args=(--data-urlencode "url=${webhook_url}")
    for e in "${events[@]}"; do args+=(--data-urlencode "enabled_events[]=${e}"); done
    stripe_patch "/webhook_endpoints/${endpoint_id}" "${args[@]}" >/dev/null
    echo "webhook endpoint: enabled_events synced (${#events[@]} events)"
  fi
else
  echo "webhook endpoint: creating"
  if [[ "${DRY_RUN}" == "false" ]]; then
    args=(--data-urlencode "url=${webhook_url}")
    for e in "${events[@]}"; do args+=(--data-urlencode "enabled_events[]=${e}"); done
    created="$(stripe_post "/webhook_endpoints" "${args[@]}")"
    secret="$(jq -r '.secret // ""' <<<"${created}")"
    [[ -n "${secret}" ]] || {
      echo "::error::failed to create webhook endpoint: $(jq -c . <<<"${created}")" >&2
      exit 1
    }
    vault_key_prefix="SANDBOX"
    [[ "${ENV_NAME}" == "prod" ]] && vault_key_prefix="PROD"
    echo
    echo "############################################################"
    echo "# Webhook signing secret (shown once, Stripe will not show it"
    echo "# again — store it now as ${vault_key_prefix}_STRIPE_WEBHOOK_SECRET in Vault kv/billing-service):"
    echo "#"
    echo "#   ${secret}"
    echo "############################################################"
    echo
  fi
fi

# --- Summary ---------------------------------------------------------------
echo
echo "== billing_plans.stripe_price_id to write =="
for row in "${price_summary[@]}"; do
  plan_id="${row%%|*}"
  pid="${row#*|}"
  printf '  %-16s -> %s\n' "${plan_id}" "${pid}"
done
echo
echo "Feed these into billing_plans via the admin API (see"
echo "docs/roadmap/feature-subscription-billing-operations/05-stripe-catalog-automation.md)."
