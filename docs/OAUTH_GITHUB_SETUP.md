# GitHub OAuth 注册/登录配置指南

> 适用范围:`accounts.svc.plus`(后端)+ `console.svc.plus` / portal(前端)。
> 结论先行:**代码链路已完整,上线只缺配置**。凭据全部配在 accounts 侧,前端零凭据、只做转发。
> 最后更新:2026-07-10

## 1. 登录链路总览

```
浏览器                portal (console.svc.plus)          accounts.svc.plus                GitHub
  │  点击 GitHub 登录        │                                  │                            │
  ├────────────────────────>│ /api/auth/oauth/login/github     │                            │
  │                         ├── 307 (带 frontend_url) ────────>│ /api/auth/oauth/login/github
  │                         │                                  ├── 302 (state 含 frontend_url) ──> 授权页
  │                         │                                  │<── 授权回调 code ───────────┤
  │                         │                                  │ /api/auth/oauth/callback/github
  │                         │                                  │  换 token → 拉 profile →
  │                         │                                  │  按邮箱自动注册(+7天 trial)→
  │                         │                                  │  绑定 identity → 建 session →
  │                         │                                  │  签发一次性 exchange_code
  │<── 307 {frontend}/login?exchange_code=... ─────────────────┤
  │                         │                                  │
  ├────────────────────────>│ POST /api/auth/token/exchange    │
  │                         ├── 转发换正式 session token ─────>│
  │<── 登录完成,跳转首页 ──┤                                  │
```

关键代码位置:

| 环节 | 文件 |
|---|---|
| portal 登录按钮转发 | `portal/src/app/api/auth/oauth/login/[provider]/route.ts` |
| 后端 login/callback | `accounts.svc.plus/api/api.go` (`oauthLogin` / `oauthCallback`) |
| Provider 实现 | `accounts.svc.plus/internal/auth/oauth.go` |
| frontend_url 校验白名单 | `accounts.svc.plus/api/xworkmate.go` (`validateFrontendURL`) |
| provider 装配(读配置) | `accounts.svc.plus/cmd/accountsvc/main.go`(约 L1126 起) |
| exchange_code 消费 | `portal/src/app/(auth)/login/LoginContent.tsx` + `portal/src/app/api/auth/token/exchange/route.ts` |

## 2. GitHub 侧:注册 OAuth App

打开 <https://github.com/settings/developers> → **OAuth Apps** → **New OAuth App**:

| 字段 | 值 |
|---|---|
| Application name | `svc.plus Console`(随意) |
| Homepage URL | `https://console.svc.plus` |
| Authorization callback URL | `https://accounts.svc.plus/api/auth/oauth/callback/github` |

创建后记录 **Client ID**;点 **Generate a new client secret** 获取 **Client Secret**(只显示一次)。

注意事项:

- ⚠️ callback URL 填的是 **accounts 后端**地址,不是 console 前端地址 —— 最常见的填错点。
- 一个 GitHub OAuth App 只支持一个 callback URL。本地调试请**另建一个 dev App**,callback 填 `http://localhost:8080/api/auth/oauth/callback/github`(accounts 本地端口)。
- 权限 scope 由代码写死为 `user:email` + `read:user`,GitHub App 侧无需额外配置。

## 3. accounts.svc.plus 侧(核心)

> **部署链路先看清**(2026-07-10 勘误):线上 accounts.svc.plus 实际走 **GitHub Actions pipeline → x-evor/playbooks `deploy_accounts_svc_plus.yml` → VPS docker compose**,配置由 `roles/vhosts/accounts_service/templates/account.yaml.j2` + `app.env.j2` 渲染,凭据来自 GitHub Actions secrets(源头存 Vault)。**`deploy/gcp/cloud-run/` 与 `config/account.cloudrun.yaml` 是 Cloud Run 路线的配置,当前未启用**;§3.1–3.3 中 gcloud / Secret Manager 相关内容仅在走 Cloud Run 时适用。
>
> **VPS 路线的真实缺口**:playbooks 的 `account.yaml.j2` 已含 `auth.enable: true` + OAuth 占位符,但 `app.env.j2` / role defaults / pipeline deploy env 均未注入 `GITHUB_CLIENT_ID`、`GITHUB_CLIENT_SECRET`、`AUTH_TOKEN_*` —— envsubst 渲染为空,导致 provider 未装配(线上 404 的真正根因;已在 install.svc.plus 主机的 `/opt/cloud-neutral/accounts/managed/prod/env/app.env` 实地确认)。
>
> **修复(2026-07-10,playbooks 分支 `accounts-oauth-env` + accounts pipeline.yml)**:
> 1. `app.env.j2` 增加 `AUTH_TOKEN_*`、`GITHUB_CLIENT_ID/SECRET`、`GOOGLE_CLIENT_ID/SECRET` 行
> 2. `defaults/main.yml` 的 `accounts_service_env_defaults` 增加对应 env lookup;CI 侧变量名用 `OAUTH_GITHUB_*` 前缀(**GitHub Actions 禁止 secret/env 以 `GITHUB_` 开头**),由 role 映射回容器内的 `GITHUB_CLIENT_*`
> 3. `account.yaml.j2` 修复 token 段 `${VAR:-default}` 写法(envsubst 不支持,此前线上 JWT 签名密钥实为字面量垃圾串);`frontendUrl` 改为 Jinja 直渲 `{{ accounts_service_frontend_url }}`;`redirectUrl` 拆为 per-provider 硬编码
> 4. accounts 仓库 `pipeline.yml` deploy step 透传 `OAUTH_GITHUB_CLIENT_ID`(明文)+ 4 个 `${{ secrets.* }}`
> 5. GitHub 仓库 secrets 需补录:`OAUTH_GITHUB_CLIENT_SECRET`、`AUTH_TOKEN_PUBLIC_TOKEN`、`AUTH_TOKEN_REFRESH_SECRET`、`AUTH_TOKEN_ACCESS_SECRET`(源头存 Vault,见 §3.2)

### 3.1 补配置模板(Cloud Run 路线,当前未启用)

Cloud Run 部署使用 `CONFIG_TEMPLATE=/app/config/account.cloudrun.yaml`,该文件此前**没有 `auth:` 段**,导致 provider map 为空,`/api/auth/oauth/login/github` 返回 `404 provider_not_found`。已补上(2026-07-10):

```yaml
auth:
  enable: true
  token:
    publicToken: "${AUTH_TOKEN_PUBLIC_TOKEN}"
    refreshSecret: "${AUTH_TOKEN_REFRESH_SECRET}"
    accessSecret: "${AUTH_TOKEN_ACCESS_SECRET}"
    accessExpiry: "1h"
    refreshExpiry: "168h"
  oauth:
    frontendUrl: "https://console.svc.plus"
    github:
      clientId: "${GITHUB_CLIENT_ID}"
      clientSecret: "${GITHUB_CLIENT_SECRET}"
      redirectUrl: "https://accounts.svc.plus/api/auth/oauth/callback/github"
    google:
      clientId: "${GOOGLE_CLIENT_ID}"
      clientSecret: "${GOOGLE_CLIENT_SECRET}"
      redirectUrl: "https://accounts.svc.plus/api/auth/oauth/callback/google"
```

两个关键约束(踩坑记录):

- ⚠️ **模板由 `entrypoint.sh` 用 `envsubst` 渲染,不支持 `${VAR:-default}` 默认值语法** —— 写了会原样留在渲染结果里变成垃圾字符串(`config/account.yaml` 里的 `:-` 默认值写法实际从未生效过)。cloudrun 模板只用纯 `${VAR}`,固定值直接硬编码。
- ⚠️ **`auth.enable: true` 会激活 TokenService**,给 `/api/auth` 受保护组、`/api`(admin)、`/api/overlay` 挂上 `AuthMiddleware`(此前这些组无中间件,靠 handler 自行校验)。已确认兼容:middleware 接受 Bearer JWT **或** DB session token 回退,portal 全部以 `Authorization: Bearer <session-token>` 调用,cookie 名 `xc_session` 两边一致。副作用是未带凭据的请求会在 middleware 层被 401(实为安全加固)。

### 3.2 密钥清单(源头统一存 Vault)

本次新增、需要入 Vault(建议路径 `kv/accounts.svc.plus`)再同步到 GitHub Actions secrets 的条目:

| Vault 字段 | 说明 | 生成方式 |
|---|---|---|
| `GITHUB_CLIENT_SECRET` | GitHub OAuth App 的 Client Secret | GitHub App 页面生成,只显示一次 |
| `AUTH_TOKEN_PUBLIC_TOKEN` | TokenService 公共 token | `openssl rand -base64 32` |
| `AUTH_TOKEN_REFRESH_SECRET` | JWT refresh 签名密钥 | `openssl rand -base64 32` |
| `AUTH_TOKEN_ACCESS_SECRET` | JWT access 签名密钥 | `openssl rand -base64 32` |
| `GOOGLE_CLIENT_SECRET` | (开 Google 登录时)Google OAuth Client Secret | GCP OAuth 同意屏幕 |

**不是密钥、不进 Vault** 的配置项(直接写在 playbooks defaults / pipeline env):`GITHUB_CLIENT_ID`(`Ov23lioecyD2bjWNQJr0`,公开值)、`OAUTH_REDIRECT_URL`(`https://accounts.svc.plus/api/auth/oauth/callback/github`)、`OAUTH_FRONTEND_URL`(`https://console.svc.plus`)。

既有已在链路上的密钥(应确认 Vault 已有记录,不必新建):`ACCOUNT_PG_PASSWORD`、`BRIDGE_AUTH_TOKEN`、`BRIDGE_REVIEW_AUTH_TOKEN`、`INTERNAL_SERVICE_TOKEN`、`SMTP_USERNAME`/`SMTP_PASSWORD`、`REVIEW_ACCOUNT_PASSWORD`、`SINGLE_NODE_VPS_SSH_PRIVATE_KEY`、`WORKSPACE_REPO_TOKEN`、`GHCR_TOKEN`、`XWORKMATE_VAULT_TOKEN`。

### 3.2.1 GitHub Actions 仓库 secrets/vars(部署实际读取处)

pipeline 从 `ai-workspace-services/accounts` 的仓库 secrets 取值(**不是** Vault 直读;Vault 是源头,需人工同步)。新仓库迁移后缺口较大 —— 下表为 pipeline 引用但仓库可能未配的项:

| 仓库 secret | Vault 源 | 必需性 |
|---|---|---|
| `SINGLE_NODE_VPS_SSH_PRIVATE_KEY` | `kv/CICD` 同名字段 | 必需(缺失 → SSH prep step 退出 1) |
| `SSH_KNOWN_HOSTS` | `kv/CICD` | 必需 |
| `WORKSPACE_REPO_TOKEN` | 能 checkout 私有 playbooks 仓库的 PAT | 必需(playbooks 私有时) |
| `OAUTH_GITHUB_CLIENT_SECRET` | `kv/accounts.svc.plus` → `GITHUB_CLIENT_SECRET` | 必需 |
| `AUTH_TOKEN_PUBLIC_TOKEN` | `kv/accounts.svc.plus` | 必需 |
| `AUTH_TOKEN_REFRESH_SECRET` | `kv/accounts.svc.plus` | 必需 |
| `AUTH_TOKEN_ACCESS_SECRET` | `kv/accounts.svc.plus` | 必需 |
| `GHCR_TOKEN` | GHCR PAT | 可选(缺省回退 `github.token`) |

仓库 **variables**(非密钥):`OAUTH_GITHUB_CLIENT_ID`(= `Ov23lioecyD2bjWNQJr0`,已设);可选 `IMAGE_REPO_OWNER`、`GHCR_USERNAME`(缺省取仓库 owner)。

> ⚠️ **CI 侧 secret/var 不能以 `GITHUB_` 开头**(Actions 保留前缀),故用 `OAUTH_GITHUB_*`,由 playbooks role 在 `defaults/main.yml` 映射回容器内的 `GITHUB_CLIENT_*`。

设置命令:

```bash
gh secret set SINGLE_NODE_VPS_SSH_PRIVATE_KEY -R ai-workspace-services/accounts < /path/to/ssh_key
gh secret set OAUTH_GITHUB_CLIENT_SECRET     -R ai-workspace-services/accounts   # 交互粘贴
gh secret set AUTH_TOKEN_PUBLIC_TOKEN        -R ai-workspace-services/accounts
gh secret set AUTH_TOKEN_REFRESH_SECRET      -R ai-workspace-services/accounts
gh secret set AUTH_TOKEN_ACCESS_SECRET       -R ai-workspace-services/accounts
# SSH_KNOWN_HOSTS / WORKSPACE_REPO_TOKEN 同理
```

### 3.2.2 gcloud/Secret Manager(仅 Cloud Run 路线,当前未启用,跳过)

以下命令仅走 Cloud Run 时适用:

```bash
echo -n "<github-client-secret>" | gcloud secrets create github-client-secret --data-file=-
openssl rand -base64 32 | tr -d '\n' | gcloud secrets create auth-token-public --data-file=-
openssl rand -base64 32 | tr -d '\n' | gcloud secrets create auth-token-refresh-secret --data-file=-
openssl rand -base64 32 | tr -d '\n' | gcloud secrets create auth-token-access-secret --data-file=-

for s in github-client-secret auth-token-public auth-token-refresh-secret auth-token-access-secret; do
  gcloud secrets add-iam-policy-binding "$s" \
    --member="serviceAccount:266500572462-compute@developer.gserviceaccount.com" \
    --role="roles/secretmanager.secretAccessor"
done
```

### 3.3 Cloud Run 服务注入环境变量

`deploy/gcp/cloud-run/prod-service.yaml` 的 `accounts-api` 容器 `env:` 已追加(2026-07-10):

```yaml
- name: GITHUB_CLIENT_ID
  value: "Ov23lioecyD2bjWNQJr0"   # Client ID 不敏感,可明文
- name: GITHUB_CLIENT_SECRET
  valueFrom:
    secretKeyRef: { name: github-client-secret, key: latest }
- name: AUTH_TOKEN_PUBLIC_TOKEN
  valueFrom:
    secretKeyRef: { name: auth-token-public, key: latest }
- name: AUTH_TOKEN_REFRESH_SECRET
  valueFrom:
    secretKeyRef: { name: auth-token-refresh-secret, key: latest }
- name: AUTH_TOKEN_ACCESS_SECRET
  valueFrom:
    secretKeyRef: { name: auth-token-access-secret, key: latest }
```

preview 环境同理修改 `preview-service.yaml`(callback URL 需对应 preview 域名,并使用单独的 GitHub OAuth App)。

### 3.4 重新部署并验证

```bash
curl -si "https://accounts.svc.plus/api/auth/oauth/login/github" | head -5
```

- ✅ 配置生效:`307` + `Location: https://github.com/login/oauth/authorize?...`
- ❌ 未生效:`404 {"error":"provider_not_found"}` → 检查 3.1–3.3

## 4. console / portal 侧:基本零配置

- **不需要任何 GitHub 凭据** —— 登录按钮只是 307 转发到 accounts。
- `ACCOUNT_SERVICE_URL` 默认 fallback 即 `https://accounts.svc.plus`(见 `portal/src/server/serviceConfig.ts`),生产不配也可工作;若已配置,确认指向正确。
- accounts 的 CORS `allowedOrigins` 已包含 `https://console.svc.plus`,无需改动。

## 5. 端到端验证

1. 浏览器打开 `https://console.svc.plus/login`
2. 点击 GitHub 登录 → 跳转 GitHub 授权页
3. 授权后落回 `https://console.svc.plus/login?exchange_code=...`
4. 页面自动 POST `/api/auth/token/exchange` 换 session,跳转首页
5. 新用户自动注册,`EmailVerified=true`,并附带 7 天 trial 订阅(`PlanID: TRIAL-7D`)

## 6. 已知遗留问题(不阻塞上线)

| 问题 | 说明 | 建议 |
|---|---|---|
| CSRF state 未校验 | `oauthLogin` 生成 nonce,但 callback 只解析 state 里的 frontend_url,从不比对 nonce(代码注释自认) | 开放注册前修复:nonce 存 HttpOnly cookie,callback 比对 |
| `frontend_url` 取到容器 hostname | portal route 用 `request.nextUrl.origin`,容器内直连时得到 Docker 容器 ID(如 `https://302d8d7d135c:3000`);后端白名单会拒绝并回退 `OAUTH_FRONTEND_URL`,导致 dev 登录后被弹回 console.svc.plus | portal 优先读配置的公网 URL 或 `X-Forwarded-Host`;生产经代理访问不受影响 |
| identity 未用于登录匹配 | callback 仅按 email 查用户;用户更换 GitHub 主邮箱会被当新用户重新注册 | 先按 `(provider, external_id)` 查 identity 表,再回退 email |

## 7. 排障速查

| 现象 | 原因 | 处理 |
|---|---|---|
| `404 provider_not_found` | 配置模板无 `auth:` 段 / 未设 `GITHUB_CLIENT_ID` | 见 §3.1–3.3 |
| GitHub 报 `redirect_uri_mismatch` | OAuth App callback URL 与 `redirectUrl` 配置不一致 | 两边对齐,注意 http/https 与端口 |
| `email_not_verified` (401) | GitHub 账号主邮箱未验证 | 用户需在 GitHub 验证邮箱 |
| `email_missing` (400) | profile 拿不到邮箱(罕见,私有邮箱兜底已实现) | 检查 scope 是否被改动 |
| 登录后落到 console.svc.plus 而非 dev 前端 | `frontend_url` 白名单拒绝了 dev 域名 | 本地用 `localhost:3000`(在白名单内),或见 §6 第 2 条 |
| `invalid_exchange_code` | exchange_code 过期(一次性、短 TTL)或重复使用 | 重新走登录流程 |
