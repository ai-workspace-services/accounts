# Stripe Live Vault KV 路径与权限配置手册

## 1. 概述与链路模型

`console.svc.plus` 仅作为前端与 BFF 层提交 `planId` 和公开的 `stripePriceId`，**严禁**保存任何 `sk_live_`、`sk_test_` 或 `whsec_` 密钥。生产环境（`prod-k3s-v6`）的 Stripe Live 结账会话（`cs_live_...`）与 Webhook 签名校验全部在 `accounts.svc.plus`（`web-saas-accounts`）服务端完成。

```text
Console / Portal (https://console.svc.plus)
  ↓ 提交 planId / stripePriceId（无密钥）
Accounts (https://accounts.svc.plus, namespace: web-saas-prod)
  ← ExternalSecret/accounts-stripe ← Vault kv/data/prod/accounts/runtime
  ↓ 创建 Checkout Session (cs_live_...)
Stripe Live (acct_1SvuMHLIhVa2N0n8)
  ↓ Webhook POST https://accounts.svc.plus/api/billing/stripe/webhook
Accounts 校验 STRIPE_WEBHOOK_SECRET (whsec_...) 并激活订阅与配额
```

---

## 2. Vault KV 路径与字段清单

Vault 服务地址：`https://vault.svc.plus`（KV v2 挂载点：`kv/`）。

### 2.1 运行时消费路径（K3s 集群 Pod 读取）

- **Vault CLI 路径**：`kv/prod/accounts/runtime`
- **Vault API / Policy 路径**：`kv/data/prod/accounts/runtime`
- **K8s 映射资源**：`web-saas-prod` 命名空间下的 `ExternalSecret/accounts-stripe` → `Secret/accounts-stripe`（刷新周期 `5m`）

| 字段名 (Property) | 示例 / 格式约束 | 用途说明 |
| --- | --- | --- |
| `STRIPE_MODE` | `live` | 显式声明运行时模式，必须与 `STRIPE_SECRET_KEY` 前缀一致（`live` 对应 `sk_live_...` / `rk_live_...`；`sandbox` / `test` 对应 `sk_test_...`） |
| `STRIPE_SECRET_KEY` | `sk_live_...` | Stripe Live Secret Key，用于创建结账会话、预览变更与查询订阅/发票 |
| `STRIPE_WEBHOOK_SECRET` | `whsec_...` | Stripe Live Webhook Endpoint signing secret，用于验证 `/api/billing/stripe/webhook` 请求签名 |
| `STRIPE_PORTAL_CONFIGURATION_ID` | `bpc_...` 或空字符串 `""` | 可选的 Stripe Billing Portal 配置 ID |

> 安全约束：`STRIPE_API_BASE_URL` 在 Live 模式下禁止覆盖（参见 `accounts/api/stripe.go` 中 `resolveStripeAPIBaseURL`）。

### 2.2 编排与目录同步源路径（CI/CD 与 Catalog Bootstrap 读取）

- **Vault CLI 路径**：`kv/prod/billing-service`
- **Vault API / Policy 路径**：`kv/data/prod/billing-service`
- **消费者**：`platform-ops-toolkit/.github/workflows/selfhost-orchestrator.yml`（`[4] Stripe catalog`）与 `scripts/serverless_uat/bootstrap_stripe_catalog.sh`

| 字段名 (Property) | 示例 / 格式约束 | 用途说明 |
| --- | --- | --- |
| `PROD_STRIPE_SECRET_KEY` | `sk_live_...` | 生产 Stripe Live Secret Key 主源 |
| `PROD_STRIPE_WEBHOOK_SECRET` | `whsec_...` | 生产 Stripe Live Webhook signing secret 主源 |
| `PROD_STRIPE_WEBHOOK_URL` | `https://accounts.svc.plus/api/billing/stripe/webhook` | 生产 Stripe Webhook Endpoint URL |
| `SANDBOX_STRIPE_SECRET_KEY` | `sk_test_...` | 沙盒测试密钥（仅非生产或测试对账使用） |
| `SANDBOX_STRIPE_WEBHOOK_SECRET` | `whsec_...` | 沙盒测试 Webhook signing secret |

> 同步脚本还需读取 `kv/data/CICD` 中的 `ROOT_BOOTSTRAP_EMAIL` 与 `ROOT_BOOTSTRAP_PASSWORD`，用于换取短期管理员 JWT 调用 `PUT /api/auth/admin/billing/plans/:planId` 回写 `stripe_price_id`。

---

## 3. 所需权限矩阵（Vault ACL / JWT / Kubernetes RBAC）

### 3.1 K3s 工作负载运行时读取权限（最小权限）

宣言位置：
- Workload Identity：[gitops/operations/workload-identities/prod-k3s-v6/web-saas-secrets-reader.yaml](https://github.com/ai-workspace-infra/gitops/blob/main/operations/workload-identities/prod-k3s-v6/web-saas-secrets-reader.yaml)
- SecretStore / ExternalSecret：[gitops/services/prod-k3s-v6/web-saas-prod/vault-secrets.yaml](https://github.com/ai-workspace-infra/gitops/blob/main/services/prod-k3s-v6/web-saas-prod/vault-secrets.yaml)

| 层面 | 资源 / 配置项 | 要求的值与权限 |
| --- | --- | --- |
| **Kubernetes ServiceAccount** | `web-saas-prod/web-saas-secrets-reader` | `automountServiceAccountToken: false`；仅允许 `external-secrets` 命名空间的 `ServiceAccount/external-secrets` 为其签发短期 token（`RoleBinding/web-saas-secrets-reader`，`verbs: ["create"]` on `serviceaccounts/token`） |
| **Kubernetes SecretStore** | `web-saas-prod/web-saas-secrets-reader` | JWT Auth 挂载路径 `jwt-prod-k3s-prod-k3s-v6`，role `prod-k3s-v6-web-saas-secrets-reader`，audience `vault-prod-k3s-v6-web-saas-secrets-reader`，`expirationSeconds: 600` |
| **Vault ACL Policy** | `prod-k3s-v6-web-saas-secrets-reader` | 仅授予 `kv/data/prod/accounts/runtime` 与 `kv/data/prod/platform/smtp/google` 的 **`read`** 权限；明确禁止访问 `kv/data/prod/database/postgresql/admin` 与 `kv/data/prod/billing-service` |

### 3.2 CI/CD 与目录同步编排权限（GitHub OIDC）

宣言位置：
- Vault JWT Role：[gitops/security/vault/roles/github-actions-platform-ops-toolkit-prod.yaml](https://github.com/ai-workspace-infra/gitops/blob/main/security/vault/roles/github-actions-platform-ops-toolkit-prod.yaml)
- Vault ACL Policy：[gitops/security/vault/policies/github-actions-platform-ops-toolkit-prod.yaml](https://github.com/ai-workspace-infra/gitops/blob/main/security/vault/policies/github-actions-platform-ops-toolkit-prod.yaml)

| 层面 | 资源 / 配置项 | 要求的值与权限 |
| --- | --- | --- |
| **Vault JWT Role** | `github-actions-platform-ops-toolkit-prod` | `bound_audiences: ["vault"]`；限定 `repository: ai-workspace-infra/platform-ops-toolkit`、授权 workflow（如 `selfhost-orchestrator.yml`）以及生产 ref（`refs/tags/v*`、`refs/heads/release/v*`） |
| **Vault ACL Policy** | `github-actions-platform-ops-toolkit-prod` | - `kv/data/prod/*`：`create`, `read`, `update`, `list`<br>- `kv/metadata/prod/*`：`list`, `read`<br>- `kv/data/CICD` & `kv/metadata/CICD`：`read` / `list, read`（读取 `ROOT_BOOTSTRAP_*`） |
| **Accounts 管理员权限** | `POST /api/auth/login` → `PUT /api/auth/admin/billing/plans/:planId` | `ROOT_BOOTSTRAP_EMAIL` 账号须为无 MFA 挑战的服务初始化管理员（仅限脚本内临时内存态使用，不可落盘） |

### 3.3 Workload Identity 管理员同步权限（配置 `web-saas-secrets-reader` 自身）

- 角色 / 策略：`github-actions-platform-ops-toolkit-prod-k3s-v6-service-secret-admin`
- 允许管理范围：仅限声明的 `sys/policies/acl/prod-k3s-v6-web-saas-secrets-reader` 及 `auth/jwt-prod-k3s-*/role/prod-k3s-v6-web-saas-secrets-reader` 的 `read` / `create` / `update`（不允许通配或读取业务 `kv/` 数据）。

---

## 4. Stripe Live 端与产品目录前置要求

- **Stripe 账户 ID**：`acct_1SvuMHLIhVa2N0n8`（须已启用 `charges_enabled: true`）
- **Webhook Endpoint**：
  - Endpoint ID：`we_1UB4ueLIhVa2N0n8A2O7ZBJj`
  - URL：`https://accounts.svc.plus/api/billing/stripe/webhook`（禁止继续指向已下线的 `accounts-serverless-prod.svc.plus` 404 地址）
  - 必须订阅的 6 个事件：
    - `checkout.session.completed`
    - `customer.subscription.created`
    - `customer.subscription.updated`
    - `customer.subscription.deleted`
    - `invoice.paid`
    - `invoice.payment_failed`
- **产品与价格源声明**：`scripts/stripe-catalog.yaml`（同步至 `public.billing_plans`）：
  - `PLUS`（`Plus · 20 GB`，`¥20/月`）
  - `PLUS-MONTHLY-2026`（`Plus · 30 GB`，`¥30/月`）
  - `PRO-MONTHLY-V2`（`Pro 月付`，`¥50/月`）
  - `PRO-YEARLY-V2`（`Pro 年付`，`¥500/年`）
  - `CUSTOM-NODE-MONTHLY-2026`（`定制节点`，`¥80/月`）
  - `CUSTOM-AI-CONSULT-2026`（`开源解决方案/AI工具咨询`，`¥100/小时/次`，一次性 `payment` 模式）

---

## 5. 巡检、同步与轮换验证命令

> 以下所有命令均不回显原始密钥明文：

```bash
# 1. 检查 Vault KV 字段键名（禁止直接明文打印 value）
VAULT_ADDR=https://vault.svc.plus vault kv get -format=json kv/prod/accounts/runtime | jq '.data.data | keys'
VAULT_ADDR=https://vault.svc.plus vault kv get -format=json kv/prod/billing-service | jq '.data.data | keys'

# 2. 在 prod-k3s-v6 触发 ExternalSecret 强制同步并核验密钥前缀/模式
KUBECONFIG=/etc/rancher/k3s/k3s.yaml kubectl -n web-saas-prod annotate externalsecret/accounts-stripe force-sync="$(date +%s)" --overwrite
KUBECONFIG=/etc/rancher/k3s/k3s.yaml kubectl -n web-saas-prod exec deploy/web-saas-accounts-primary -- \
  sh -c 'echo "STRIPE_MODE=$STRIPE_MODE KEY_PREFIX=$(printf %s "$STRIPE_SECRET_KEY" | cut -c1-8) WH_PREFIX=$(printf %s "$STRIPE_WEBHOOK_SECRET" | cut -c1-8)"'

# 3. 校验 Webhook 路由是否由 web-saas-accounts 承接（无签名 POST 应返回 401 invalid_signature，而非 404）
curl -sS -o /dev/null -w '%{http_code}\n' -X POST https://accounts.svc.plus/api/billing/stripe/webhook -H 'Content-Type: application/json' -d '{}'

# 4. Dry-run 校验 Stripe Live 产品、价格与 Webhook
STRIPE_SECRET_KEY='从 kv/prod/billing-service#PROD_STRIPE_SECRET_KEY 读取' \
STRIPE_WEBHOOK_SECRET='从 kv/prod/billing-service#PROD_STRIPE_WEBHOOK_SECRET 读取' \
STRIPE_WEBHOOK_URL='从 kv/prod/billing-service#PROD_STRIPE_WEBHOOK_URL 读取' \
./scripts/stripe-sync-catalog.sh --env prod --domain-base svc.plus --dry-run
```

---

## 6. 多主体混合与平迁（HK + US）

如需在不中断既有 HK 收款的前提下接入 US 主体、或将新主体按业务线隔离，请参阅：
- [Stripe 多主体混合架构与平迁指南（HK + US）](./stripe-multi-entity-architecture.md)
- 模板化 Vault KV 路径：`kv/prod/payments/stripe/hk`、`kv/prod/payments/stripe/us`
- 主体专属 Webhook：`POST /api/billing/stripe/webhook/hk`、`POST /api/billing/stripe/webhook/us`（`/api/billing/stripe/webhook` 继续兼容默认主体）
