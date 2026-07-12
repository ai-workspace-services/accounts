# accounts.svc.plus

Cloud Neutral Toolkit 的账号与身份服务 (Account Service).

> A production-oriented account service for sign-in, sessions, MFA, and agent coordination.

## 部署要求 (Deployment Requirements)

| 维度 | 要求 / 规格 | 说明 |
|---|---|---|
| 网络 | 可访问的 API 域名 (可选) | 生产建议配置 `server.publicUrl` |
| 端口 | `:8080` | API 服务默认监听端口 |
| 数据库 | PostgreSQL | 存储账号/会话/状态等核心数据 |
| 缓存 (可选) | Redis | `session.cache=redis` 时需要 |
| 最低 | 1 CPU / 1GB RAM | 开发/小规模 |
| 推荐 | 2 CPU / 2GB RAM | 生产建议 |

### CI/CD 部署前置条件 (Vault JWT Role)

`.github/workflows/pipeline.yml` 的 `deploy` / `validate` job 用
`hashicorp/vault-action`（`method: jwt`，OIDC，无长期 GitHub secret）读取
`kv/accounts.svc.plus` 下的部署期 token：

| Vault key (`kv/data/accounts.svc.plus`) | 用途 | 映射到的部署变量 |
|---|---|---|
| `INTERNAL_SERVICE_TOKEN` | accounts → xworkmate-bridge 的服务间鉴权 | `BRIDGE_AUTH_TOKEN` |
| `BRIDGE_REVIEW_AUTH_TOKEN` | Apple 审核只读账号 `review@svc.plus` 专用 bridge token | `BRIDGE_REVIEW_AUTH_TOKEN` |

在 Vault 中必须预先配置好对应的 JWT role，否则 pipeline 在 `Validate Deploy
Secrets` 步骤会直接 fail。本机需要先装 `vault` CLI，并对目标 Vault 完成
`vault login`（或设好 `VAULT_ADDR` / `VAULT_TOKEN`）：

```bash
brew tap hashicorp/tap
brew install hashicorp/tap/vault

# accounts 服务运行时读写 xworkmate/* 密钥用的 policy —— CI 每次部署会以
# 这个 policy 铸造一个新的 runtime token 写入主机(见下)
vault policy write xworkmate-accounts - <<'EOF'
path "kv/data/xworkmate/*" {
  capabilities = ["create", "read", "update"]
}
path "kv/metadata/xworkmate/*" {
  capabilities = ["read", "list"]
}
EOF

vault policy write github-actions-accounts - <<'EOF'
path "kv/data/accounts.svc.plus" {
  capabilities = ["read"]
}
path "kv/metadata/accounts.svc.plus" {
  capabilities = ["read", "list"]
}

# 允许 CI 为 accounts 服务铸造 orphan runtime token
# (sudo 同时授权 orphan 创建与跨 policy 赋权)
path "auth/token/create-orphan" {
  capabilities = ["create", "update", "sudo"]
}
EOF

vault write auth/jwt/role/github-actions-accounts - <<'EOF'
{
  "role_type": "jwt",
  "user_claim": "repository",
  "bound_audiences": ["vault"],
  "bound_claims_type": "glob",
  "bound_claims": {
    "repository": "ai-workspace-services/accounts",
    "sub": "repo:ai-workspace-services/accounts:*"
  },
  "token_policies": ["github-actions-accounts"],
  "token_ttl": "20m",
  "token_max_ttl": "30m"
}
EOF
```

`workflow_dispatch` 里的 `secrets.BRIDGE_AUTH_TOKEN` /
`secrets.BRIDGE_REVIEW_AUTH_TOKEN` 仅作为 Vault 读取失败时的 fallback，长期
应以 Vault 值为准。

**手动触发时的 Vault fallback**：OIDC role（`github-actions-accounts`）失效或
需要临时排障时，`workflow_dispatch` 提供两个可选 input：

| Input | 作用 |
|---|---|
| `vault_addr` | 覆盖默认 `https://vault.svc.plus`；只在 `vault_token` 也填了才生效 |
| `vault_token` | 提供后 `Load Vault secrets` 改用 `method: token` 直接鉴权，跳过 OIDC role |

⚠️ **`vault_token` 会明文出现在这次 run 的 Inputs 摘要里** —— GitHub 不会像
对待 repo/org secret 那样自动打码 `workflow_dispatch` 的手动输入，pipeline
里加的 `::add-mask::` 只能防止它出现在后续步骤日志中，防不住触发页本身的
回显。因此：

- 只在 OIDC role 确认坏掉、需要临时验证时用，不要作为常规部署方式；
- 用完后到 Vault 里 revoke 这个 token；
- 优先修 `github-actions-accounts` role 本身（policy / bound_claims 见上），
  而不是长期依赖这个 fallback。

**运行时 token 自动轮换**：`XWORKMATE_VAULT_TOKEN`（accounts 服务运行时读取
`xworkmate/*` 密钥用的 Vault token）**不需要手工管理**。deploy job 的
`Mint Runtime Vault Token` 步骤在每次部署时用 OIDC job token 通过
`auth/token/create-orphan` 铸造一个 orphan periodic token（`period: 768h`，
policy `xworkmate-accounts`），playbook 将其写入主机 `app.env` 并重建容器。
注意：

- 铸造失败会**直接 fail 整个 deploy**（防止带着失效 token 静默上线——这正是
  2026-07-12 review 账号同步事故的根源），所以合并该 pipeline 前必须先在
  Vault 里执行上面的两个 `vault policy write`；
- token 有效期 32 天（768h），每次部署都会换新——只要部署间隔不超过 32 天
  就永远新鲜；若长时间不部署，手动触发一次 `workflow_dispatch` 即可续期；
- 旧 token 不主动 revoke，到期自然失效。

## 快速开始 (Quickstart)

### 一键初始化 (Setup Script)

```bash
curl -fsSL "https://raw.githubusercontent.com/cloud-neutral-toolkit/accounts.svc.plus/main/scripts/setup.sh?$(date +%s)" \
  | bash -s -- accounts.svc.plus --mode process --deploy
```

Docker 部署模式：

```bash
curl -fsSL "https://raw.githubusercontent.com/cloud-neutral-toolkit/accounts.svc.plus/main/scripts/setup.sh?$(date +%s)" \
  | bash -s -- accounts.svc.plus --mode docker --deploy
```

Cloud Run 部署模式：

```bash
curl -fsSL "https://raw.githubusercontent.com/cloud-neutral-toolkit/accounts.svc.plus/main/scripts/setup.sh?$(date +%s)" \
  | bash -s -- accounts.svc.plus --mode cloudrun
```

单机 `process` / `docker` 模式默认会写入 Caddy 站点配置到 `/etc/caddy/conf.d/accounts.svc.plus.conf`，并反向代理到本机 `127.0.0.1:8080`。

### 本地运行 (Local Dev)

```bash
cp .env.example .env
make dev
```

## 提交前同步要求 (Pre-Commit Sync Requirement)

控制仓库中的 `subrepos/accounts.svc.plus` 在每次提交前，必须先同步当前线上运行实例。

线上实例命名规则：

- `<server-name>-<hostname-or-env>-<git-commit-short-id>.<domain>`

例如：

- `accounts-us-xhttp-2886a64.svc.plus`

执行要求：

```bash
cd /Users/shenlan/workspaces/cloud-neutral-toolkit/github-org-cloud-neutral-toolkit/subrepos/accounts.svc.plus

# 1. 确认线上当前运行 revision / image
ssh root@us-xhttp.svc.plus 'docker ps --format "table {{.Names}}\t{{.Image}}\t{{.RunningFor}}" | grep accounts'

# 2. 动态定位当前 active accounts 实例并核对 compose 目录与镜像 tag
ssh root@us-xhttp.svc.plus '
name=$(docker ps --format "{{.Names}}" | grep "^accounts-" | head -n 1) &&
echo "$name" &&
sed -n "1,80p" "/opt/cloud-neutral/accounts/${name}/docker-compose.yml"
'

# 3. 再开始本地提交
git status
```

如果线上 revision 已变化，应先以新的 `<server-name>-<hostname-or-env>-<git-commit-short-id>.<domain>` 实例为准完成同步，再提交本地改动。

## Stripe 配置 (Stripe Billing Setup)

Stripe 相关服务端能力现在由 `accounts.svc.plus` 承担，包括：

- Checkout Session 创建
- Customer Portal 跳转
- Webhook 验签与订阅状态回写

需要的环境变量：

| 变量 | 用途 |
| --- | --- |
| `STRIPE_SECRET_KEY` | Stripe API secret key |
| `STRIPE_WEBHOOK_SECRET` | Stripe webhook endpoint secret |
| `STRIPE_ALLOWED_PRICE_IDS` | 允许下单的 `price_...` 白名单，逗号分隔 |

联调说明见 `docs/usage/stripe-billing.md`。

## 核心特性 & 技术栈 (Features & Tech Stack)

核心特性：
- 账号体系：注册/登录/会话/角色与权限
- 安全能力：邮件验证、TOTP MFA（可选）
- Agent 协同：与节点/控制面协作的同步与状态上报
- 多部署形态：本地/VM、Docker、Cloud Run（含 stunnel sidecar 示例）

技术栈：
- Go + Gin
- PostgreSQL (primary store)
- Redis (optional session cache)
- stunnel (optional secure DB connectivity; Cloud Run example included)

## 说明文档 (Docs)

- 文档入口：`docs/README.md`
- 快速开始：`docs/getting-started/quickstart.md`
- 配置说明：`docs/usage/config.md`
- Stripe 联调：`docs/usage/stripe-billing.md`
- 部署方式：`docs/usage/deployment.md`
- API 参考：`docs/api/overview.md`
- 运维：`docs/operations/monitoring.md`, `docs/operations/troubleshooting.md`
- Runbooks：`docs/Runbook/README.md`
