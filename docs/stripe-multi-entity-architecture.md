# Stripe 多主体混合架构与平迁指南（HK + US）

本文档说明 `accounts`、`portal` 与 `gitops` 如何同时支持：
1. **平迁（Single-Entity Cutover）**：通过 Vault KV 模板与 ESO 参数化，把默认 Stripe 主体从 HK（`acct_1SvuMHLIhVa2N0n8`）平滑切换到 US。
2. **多公司业务线并存（Multi-Entity Hybrid）**：在同一套 `accounts` + `console` 中同时挂载 HK 与 US 两个 Stripe 独立商户主体，按套餐（`billing_plans.features.stripe_profile`）路由 Checkout / 升级 / 续费 / Portal / Webhook / 退款，避免跨公司主体混账。

---

## 1. 核心设计原则

1. **零数据库破坏性迁移**：
   - 主体归属直接存放在现有 JSONB 字段：
     - `billing_plans.features`：`stripe_profile`、`business_line`、`merchant_entity`
     - `subscriptions.meta`：`stripe_profile`、`stripe_account_id`
   - Webhook 幂等去重表 `stripe_webhook_events` 复用主键 `event_id`：
     - 默认主体（通常 `hk`）沿用原始 `evt_...`（兼容既有已处理记录）；
     - 非默认主体（如 `us`）使用 `<profile>:<event_id>`（例如 `us:evt_...`），杜绝跨主体事件 ID 冲突或绕过重放。
   - 不改动 `cmd/accountsvc/main.go`（保持 native schema source manifest 校验和不变）及 `full_business_contract.json`（保持 81 张业务表契约不变）。

2. **安全边界（Strict Isolation）**：
   - 每个 `stripeClient` 实例拥有独立的 `secretKey`、`webhookSecret`、`accountID`、`merchantEntity`、`allowedPriceID`、`portalConfigurationID` 与 Quote HMAC 密钥上下文。
   - 不允许将现有 `hk` 订阅直接通过 PRORATION / Schedule 升级或续费到 `us` 套餐；跨主体变更会被后端拒绝（需到期/取消后重新在新主体下单）。
   - `GET /api/auth/admin/billing/stripe-profiles` 仅返回脱敏摘要（`profile`、`accountId`、`merchantEntity`、`mode`、`liveMode`、`enabled`、`webhookPath`、`isDefault`），**绝不返回** `sk_*` 或 `whsec_*`。

---

## 2. 环境变量与 Vault KV 模板化

### 2.1 运行时配置优先级

`accounts` 保留既有单一 `STRIPE_*` 环境变量作为默认主体兼容回退，同时支持通过 `STRIPE_PROFILES_FILE` 或 `STRIPE_PROFILES_JSON` 声明多个具名主体（默认主体名由 `STRIPE_DEFAULT_PROFILE` 控制，未设置时默认为 `hk`）：

```json
{
  "defaultProfile": "hk",
  "profiles": {
    "hk": {
      "accountId": "acct_1SvuMHLIhVa2N0n8",
      "merchantEntity": "HK Entity Ltd",
      "mode": "live",
      "secretKey": "sk_live_hk_...",
      "webhookSecret": "whsec_hk_...",
      "frontendUrl": "https://console.svc.plus",
      "portalConfigurationId": "bpc_hk_..."
    },
    "us": {
      "accountId": "acct_us_...",
      "merchantEntity": "US Entity Inc",
      "mode": "live",
      "secretKey": "sk_live_us_...",
      "webhookSecret": "whsec_us_...",
      "frontendUrl": "https://console.svc.plus",
      "portalConfigurationId": "bpc_us_..."
    }
  }
}
```

> 若只切换到新主体（例如切换默认收款主体为 US），只需更新 Vault KV 与 `STRIPE_DEFAULT_PROFILE=us` 或替换对应主体 Key；若仅保留 legacy 单主体 `STRIPE_*` 环境变量，系统自动将其映射为 `defaultProfile`（`hk`）。

### 2.2 Vault KV 标准路径规划

| 主体 | Vault KV v2 路径（逻辑） | Vault API / ESO 策略路径 | 关键字段 |
|---|---|---|---|
| **既有单主体（兼容）** | `kv/prod/accounts/runtime` | `kv/data/prod/accounts/runtime` | `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_MODE`, `STRIPE_PORTAL_CONFIGURATION_ID` |
| **HK 独立主体** | `kv/prod/payments/stripe/hk` | `kv/data/prod/payments/stripe/hk` | `account_id`, `merchant_entity`, `mode`, `secret_key`, `webhook_secret`, `portal_configuration_id`, `frontend_url` |
| **US 独立主体** | `kv/prod/payments/stripe/us` | `kv/data/prod/payments/stripe/us` | `account_id`, `merchant_entity`, `mode`, `secret_key`, `webhook_secret`, `portal_configuration_id`, `frontend_url` |

配套 GitOps 示例模板位于 infra 仓库：`gitops/services/prod-k3s-v6/web-saas-prod/vault-secrets.stripe-multi-entity.example.yaml`。

---

## 3. Webhook 端点与验签路由

| 端点 | 用途 | 行为 |
|---|---|---|
| `POST /api/billing/stripe/webhook` | 兼容旧入口 | 使用 `defaultProfile`（默认 `hk`）对应的 `whsec_*` 验签，并与默认主体共享未加前缀的幂等 key |
| `POST /api/billing/stripe/webhook/hk` | HK 主体专用入口 | 严格使用 `hk` 主体凭据验签与回查 Stripe API |
| `POST /api/billing/stripe/webhook/us` | US 主体专用入口 | 严格使用 `us` 主体凭据验签与回查 Stripe API；若 `us` 未配置则返回 `503 stripe_not_configured` |

在 Stripe Live 控制台为每个商户主体配置各自的 Webhook Endpoint URL，并把对应的 `Signing secret` 写入各自 Vault KV 路径即可，无需互相覆盖。

---

## 4. 目录同步与 Ops 后台操作

### 4.1 同步指定主体的 Stripe Catalog

使用 `scripts/stripe-sync-catalog.sh --stripe-profile <hk|us>` 过滤并同步对应主体的商品/价格，同时校验对应主体的 `/api/billing/stripe/webhook/<profile>`：

```bash
STRIPE_SECRET_KEY=sk_live_us_... \
STRIPE_WEBHOOK_URL=https://accounts.svc.plus/api/billing/stripe/webhook/us \
STRIPE_WEBHOOK_SECRET=whsec_us_... \
ACCOUNTS_ADMIN_TOKEN=... \
ACCOUNTS_BASE_URL=https://accounts.svc.plus \
  scripts/stripe-sync-catalog.sh --env prod --stripe-profile us --domain-base svc.plus --write-catalog
```

### 4.2 Ops 后台配置（Portal）

进入 `https://console.svc.plus/panel/management` -> Plan 目录管理：
- 页面顶部展示所有已配置的 `Stripe 收款主体` 状态摘要（主体 ID、商户名称、Merchant Entity、Live/Test 状态、Webhook 路径）。
- 新增或编辑套餐时可指定：
  - `stripeProfile`（例如 `hk` / `us`）
  - `businessLine`（业务线标识，如 `global_hk`、`us_nodes`）
  - `merchantEntity`（对应开票/收款公司名称）
- 列表卡片会显示所属 `stripeProfile` 与 `businessLine` 徽章，支持 HKD 与 USD 币种显示。

---

## 5. 上线与平迁 Runbook

### 场景 A：保持 HK 在线并灰度启用 US 业务线（零中断）
1. 在 Vault 创建 `kv/prod/payments/stripe/hk`（同步现有 HK 凭据）和 `kv/prod/payments/stripe/us`（写入 US 凭据）。
2. 为 `web-saas-secrets-reader` `VaultWorkloadIdentity` / Vault Policy 追加 `kv/data/prod/payments/stripe/*` 只读权限。
3. 在 `gitops/services/prod-k3s-v6/web-saas-prod/` 应用参考 `vault-secrets.stripe-multi-entity.example.yaml` 生成 `stripe-profiles.json` 并挂载到 `accounts`。
4. 在 US Stripe Dashboard 创建 Endpoint：`https://accounts.svc.plus/api/billing/stripe/webhook/us`。
5. 执行 `scripts/stripe-sync-catalog.sh --stripe-profile us --write-catalog` 或在 Ops 控制台新建 `stripeProfile=us` 的专属套餐并上线。

### 场景 B：将新增默认收款从 HK 平迁到 US
1. 确保 `us` 主体已按场景 A 启用且已有老客户留在 `hk`（老客户续费/取消/Portal/退款会自动按 `subscriptions.meta.stripe_profile == "hk"` 继续路由到 HK）。
2. 将新增套餐的 `stripeProfile`（或 `STRIPE_DEFAULT_PROFILE`）切换为 `us` 即可，已有在网 HK 订阅不受任何影响。
