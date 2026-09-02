#!/usr/bin/env bash
# 幂等地把 scripts/stripe-catalog.yaml 同步到 Stripe：Product、Price 走 API
# 同步；Webhook Endpoint 必须预先由受控的 Stripe 配置流程创建。
#
# 幂等策略：
#   - Product 用目录里的 key 作为 Stripe 自定义 id；已存在就更新
#     name/description，不存在就创建。
#   - Price 在 Stripe 里创建后金额不可变，所以用 lookup_key 做存在性判断：
#     已存在就跳过创建、只核对金额是否与目录一致(不一致只警告，绝不
#     自动改价——那必须是新价格/新 key)；不存在才创建。
#   - Webhook Endpoint 的 URL 和 signing secret 均来自 Vault。脚本只校验
#     该 URL 对应的 endpoint 已存在且事件集合完整；不创建、不修改，也绝
#     不输出 signing secret。
#
# 换 Stripe 账号时，先在该账号的受控配置流程中创建 Webhook endpoint，
# 再把对应的 URL 与 signing secret 写入 Vault 后执行本脚本。
#
# 用法:
#   STRIPE_SECRET_KEY=sk_test_... \
#     STRIPE_WEBHOOK_URL=https://accounts-uat.example.com/api/billing/stripe/webhook \
#     STRIPE_WEBHOOK_SECRET=whsec_... \
#     scripts/stripe-sync-catalog.sh --env uat --domain-base onwalk.net
#
#   STRIPE_SECRET_KEY=sk_test_... STRIPE_WEBHOOK_URL=https://accounts-uat.example.com/api/billing/stripe/webhook \
#     STRIPE_WEBHOOK_SECRET=whsec_... ACCOUNTS_ADMIN_TOKEN=... \
#     ACCOUNTS_BASE_URL=https://accounts-cloudflare-uat.onwalk.net \
#     scripts/stripe-sync-catalog.sh --env uat --domain-base onwalk.net --write-catalog
#
#   加 --dry-run 只打印将要做什么，不实际调用会产生副作用的 Stripe API。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CATALOG_FILE="${SCRIPT_DIR}/stripe-catalog.yaml"
STRIPE_API_BASE="${STRIPE_API_BASE:-https://api.stripe.com/v1}"
DRY_RUN=false
WRITE_CATALOG=false
ENV_NAME=""
DOMAIN_BASE=""
ACCOUNTS_BASE_URL="${ACCOUNTS_BASE_URL:-}"
STRIPE_WEBHOOK_URL="${STRIPE_WEBHOOK_URL:-}"
STRIPE_WEBHOOK_SECRET="${STRIPE_WEBHOOK_SECRET:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env) ENV_NAME="$2"; shift 2 ;;
    --domain-base) DOMAIN_BASE="$2"; shift 2 ;;
    --catalog) CATALOG_FILE="$2"; shift 2 ;;
    --dry-run) DRY_RUN=true; shift ;;
    --write-catalog) WRITE_CATALOG=true; shift ;;
    --accounts-base-url) ACCOUNTS_BASE_URL="$2"; shift 2 ;;
    *) echo "::error::unknown argument: $1" >&2; exit 1 ;;
  esac
done

: "${STRIPE_SECRET_KEY:?STRIPE_SECRET_KEY is required (sk_test_... for sandbox, sk_live_... for a live account)}"
: "${STRIPE_WEBHOOK_URL:?STRIPE_WEBHOOK_URL is required and must come from Vault}"
: "${STRIPE_WEBHOOK_SECRET:?STRIPE_WEBHOOK_SECRET is required and must come from Vault}"
[[ -n "${ENV_NAME}" ]] || { echo "::error::--env is required (e.g. uat, prod)" >&2; exit 1; }
[[ -n "${DOMAIN_BASE}" ]] || { echo "::error::--domain-base is required (e.g. onwalk.net, svc.plus)" >&2; exit 1; }
[[ -f "${CATALOG_FILE}" ]] || { echo "::error::catalog file not found: ${CATALOG_FILE}" >&2; exit 1; }
[[ "${STRIPE_WEBHOOK_URL}" =~ ^https://[^[:space:]]+$ ]] || {
  echo "::error::STRIPE_WEBHOOK_URL must be an HTTPS URL" >&2
  exit 1
}

if [[ "${WRITE_CATALOG}" == "true" ]]; then
  : "${ACCOUNTS_ADMIN_TOKEN:?ACCOUNTS_ADMIN_TOKEN is required with --write-catalog}"
fi

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

accounts_get() {
  curl -sS \
    -H "Authorization: Bearer ${ACCOUNTS_ADMIN_TOKEN}" \
    -H "Accept: application/json" \
    "${ACCOUNTS_BASE_URL}/api/auth/admin/billing/plans"
}

accounts_put_plan() {
  local plan_id="$1"
  local payload="$2"
  curl -sS --fail-with-body \
    -X PUT \
    -H "Authorization: Bearer ${ACCOUNTS_ADMIN_TOKEN}" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${ACCOUNTS_BASE_URL}/api/auth/admin/billing/plans/${plan_id}" \
    --data "${payload}"
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

    actual_amount="${amount}"
    actual_currency="${currency}"
    actual_unit="once"
    [[ -n "${interval}" ]] && actual_unit="${interval}"
    if [[ -n "${found_id}" ]]; then
      existing_amount="$(jq -r '.data[0].unit_amount' <<<"${lookup}")"
      actual_amount="${existing_amount}"
      actual_currency="$(jq -r '.data[0].currency // empty' <<<"${lookup}")"
      existing_interval="$(jq -r '.data[0].recurring.interval // empty' <<<"${lookup}")"
      [[ -n "${existing_interval}" ]] && actual_unit="${existing_interval}"
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
        actual_amount="$(jq -r '.unit_amount // empty' <<<"${created}")"
        actual_currency="$(jq -r '.currency // empty' <<<"${created}")"
        created_interval="$(jq -r '.recurring.interval // empty' <<<"${created}")"
        [[ -n "${created_interval}" ]] && actual_unit="${created_interval}"
        [[ -n "${price_id}" ]] || {
          echo "::error::failed to create price ${qkey}: $(jq -c . <<<"${created}")" >&2
          exit 1
        }
      else
        price_id="(dry-run, not created)"
      fi
    fi
    price_summary+=("${plan_id}|${price_id}|${actual_amount}|${actual_currency}|${actual_unit}")
  done
done

# --- Webhook endpoint -----------------------------------------------------
webhook_url="${STRIPE_WEBHOOK_URL}"
mapfile -t events < <(jq -r '.webhook.events[]' <<<"${catalog_json}")

echo
echo "webhook url: ${webhook_url}"

existing_endpoints="$(stripe_get "/webhook_endpoints?limit=100")"
endpoint_id="$(jq -r --arg url "${webhook_url}" '.data[] | select(.url == $url) | .id' <<<"${existing_endpoints}" | head -1)"

if [[ -n "${endpoint_id}" ]]; then
  missing_events=()
  for e in "${events[@]}"; do
    if ! jq -e --arg event "${e}" --arg endpoint_id "${endpoint_id}" \
      '.data[] | select(.id == $endpoint_id) | .enabled_events | index($event)' \
      <<<"${existing_endpoints}" >/dev/null; then
      missing_events+=("${e}")
    fi
  done
  if (( ${#missing_events[@]} > 0 )); then
    echo "::error::configured Stripe webhook endpoint ${endpoint_id} is missing required events: ${missing_events[*]}" >&2
    exit 1
  fi
  echo "webhook endpoint: verified -> ${endpoint_id} (${#events[@]} required events present)"
else
  echo "::error::configured Stripe webhook endpoint does not exist: ${webhook_url}. Create or rotate it through the controlled Stripe configuration process, then update Vault." >&2
  exit 1
fi

# --- Write catalog price snapshots --------------------------------------
if [[ "${WRITE_CATALOG}" == "true" ]]; then
  if [[ -z "${ACCOUNTS_BASE_URL}" ]]; then
    ACCOUNTS_BASE_URL="https://accounts-${ENV_NAME}.${DOMAIN_BASE}"
    [[ "${ENV_NAME}" == "prod" ]] && ACCOUNTS_BASE_URL="https://accounts.${DOMAIN_BASE}"
  fi
  ACCOUNTS_BASE_URL="${ACCOUNTS_BASE_URL%/}"

  echo
  echo "== writing Stripe prices to accounts billing_plans =="
  if [[ "${DRY_RUN}" == "true" ]]; then
    echo "dry-run: would read ${ACCOUNTS_BASE_URL}/api/auth/admin/billing/plans"
  else
    plans_response="$(accounts_get)"
    jq -e '.plans | type == "array"' <<<"${plans_response}" >/dev/null || {
      echo "::error::accounts admin plans response is invalid: $(jq -c . <<<"${plans_response}")" >&2
      exit 1
    }
  fi

  for row in "${price_summary[@]}"; do
    IFS='|' read -r plan_id price_id amount price_currency price_unit <<<"${row}"
    [[ "${price_id}" != "(dry-run, not created)" ]] || continue
    if [[ "${DRY_RUN}" == "true" ]]; then
      echo "dry-run: ${plan_id} -> ${price_id} (${amount} ${price_currency}/${price_unit})"
      continue
    fi

    existing_plan="$(jq -c --arg plan_id "${plan_id}" '.plans[] | select(.planId == $plan_id)' <<<"${plans_response}" | head -1)"
    if [[ -z "${existing_plan}" ]]; then
      echo "::error::billing plan ${plan_id} does not exist in accounts; seed it before --write-catalog" >&2
      exit 1
    fi
    payload="$(jq -c \
      --arg plan_id "${plan_id}" \
      --arg price_id "${price_id}" \
      --arg currency "$(tr '[:lower:]' '[:upper:]' <<<"${price_currency}")" \
      --arg unit "$(tr '[:upper:]' '[:lower:]' <<<"${price_unit}")" \
      --argjson amount "${amount}" \
      --arg reason "stripe-sync-catalog.sh --env ${ENV_NAME}" \
      '.plans[] | select(.planId == $plan_id)
       | .stripePriceId = $price_id
       | .priceAmount = $amount
       | .priceCurrency = $currency
       | .priceUnit = $unit
       | .reason = $reason' <<<"${plans_response}")"
    accounts_put_plan "${plan_id}" "${payload}" >/dev/null
    echo "catalog ${plan_id}: ${price_id} (${amount} ${price_currency}/${price_unit})"
  done
fi

# --- Summary ---------------------------------------------------------------
echo
echo "== billing_plans.stripe_price_id to write =="
for row in "${price_summary[@]}"; do
  IFS='|' read -r plan_id pid amount price_currency price_unit <<<"${row}"
  printf '  %-16s -> %s (%s %s/%s)\n' "${plan_id}" "${pid}" "${amount}" "${price_currency}" "${price_unit}"
done
echo
if [[ "${WRITE_CATALOG}" == "true" ]]; then
  echo "Stripe price IDs and published price snapshots were written to accounts."
else
  echo "Feed these into billing_plans via the admin API, or rerun with --write-catalog."
fi
