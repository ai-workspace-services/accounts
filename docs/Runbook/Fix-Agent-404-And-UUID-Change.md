# 修复 Agent 404 错误和用户 UUID 变更

**日期**: 2026-02-05  
**负责人**: SRE Team  
**审核人**: DevOps Lead  
**最后更新**: 2026-02-05T15:28:00+08:00

## 问题描述

### 1. Agent 通信 404 错误
- **现象**: Agent 服务在向 `accounts-svc-plus` 报告状态时收到 404 错误
- **影响范围**: 所有 agent 节点无法正常上报心跳和配置同步
- **错误日志**:
  ```
  Feb 05 07:24:23 hk-xhttp.svc.plus agent-svc-plus[107285]: 
  {"time":"2026-02-05T07:24:23.907002669Z","level":"ERROR","msg":"xray config sync failed",
   "component":"agent-xray-sync","target":"tcp",
   "err":"list clients: controller returned 404 Not Found: 404 page not found"}
  
  POST 404 https://accounts-svc-plus-266500572462.asia-northeast1.run.app/api/agent-server/v1/status
  GET 404 https://accounts-svc-plus-266500572462.asia-northeast1.run.app/api/agent-server/v1/users
  ```

### 2. 用户 UUID 变更需求
- **用户**: tester123@example.com
- **原 UUID**: `4b66928e-a81e-4981-bae0-289ddb92439c`
- **新 UUID**: `18d270a9-533d-4b13-b3f1-e7f55540a9b2`
- **原因**: 业务需求，需要将用户 ID 更改为指定值

### 3. Agent 节点数据显示问题
- **现象**: `/panel/agent` 页面显示 "Loading control center..."
- **影响**: 用户无法查看运行节点状态

## 根本原因分析

### Agent 404 错误的根本原因

1. **代码已正确实现**：
   - `accounts.svc.plus/cmd/accountsvc/main.go` 第 1061-1070 行已注册 `/api/agent-server/v1/*` 路由
   - 包括 `GET /api/agent-server/v1/users` 和 `POST /api/agent-server/v1/status`

2. **生产环境未部署最新代码**：
   - Cloud Run 服务 `accounts-svc-plus` 运行的是旧版本代码
   - 旧版本不包含 agent API 路由
   - 测试确认：`curl https://accounts-svc-plus-266500572462.asia-northeast1.run.app/api/agent-server/v1/users` 返回 `404 page not found`

3. **Agent 配置正确**：
   - Agent 配置文件：`/etc/agent/account-agent.yaml`
   - Controller URL: `https://accounts-svc-plus-266500572462.asia-northeast1.run.app`
   - API Token: 正确配置（与 `INTERNAL_SERVICE_TOKEN` 匹配）

## 诊断步骤

### 1. 检查 Agent 日志
```bash
# 在 agent 节点上查看日志
ssh root@hk-xhttp.svc.plus
journalctl -u agent-svc-plus -n 50 --no-pager

# 发现错误
# "err":"list clients: controller returned 404 Not Found: 404 page not found"
```

### 2. 检查 Agent 配置
```bash
# 查看 agent 配置
ssh root@hk-xhttp.svc.plus "cat /etc/agent/account-agent.yaml"

# 确认 controller URL 和 token 配置正确
# controllerUrl: "https://accounts-svc-plus-266500572462.asia-northeast1.run.app"
# apiToken: "uTvryFvAbz6M5sRtmTaSTQY6otLZ95hneBsWqXu+35I="
```

### 3. 测试 API 端点
```bash
# 测试 /api/agent-server/v1/users 端点
curl -s -H "Authorization: Bearer uTvryFvAbz6M5sRtmTaSTQY6otLZ95hneBsWqXu+35I=" \
  "https://accounts-svc-plus-266500572462.asia-northeast1.run.app/api/agent-server/v1/users"

# 返回: 404 page not found
# 确认生产环境缺少该路由
```

### 4. 检查代码实现
```bash
# 检查路由注册代码
grep -n "registerAgentAPIRoutes" accounts.svc.plus/cmd/accountsvc/main.go

# 第 852 行: registerAgentAPIRoutes(r, agentRegistry, gormSource, logger)
# 第 1061 行: func registerAgentAPIRoutes(...)
# 确认代码中已正确实现
```

### 5. 检查数据库约束（UUID 变更）
```bash
# 连接到 PostgreSQL
ssh -i ~/.ssh/id_rsa root@postgresql.svc.plus

# 查看外键约束
docker exec postgresql-svc-plus psql -U postgres -d account -c "
  SELECT conname, conrelid::regclass 
  FROM pg_constraint 
  WHERE confrelid = 'public.users'::regclass;
"

# 结果显示:
# - identities_user_uuid_fkey
# - sessions_user_uuid_fkey
# - subscriptions_user_uuid_fkey
```

## 修复方案

### 修复 1: 部署最新代码到 Cloud Run ⚠️ **关键修复**

**问题**: 生产环境的 Cloud Run 服务运行的是旧版本代码，缺少 agent API 路由

**解决方案**: 重新构建和部署 `accounts-svc-plus` 服务

```bash
# 1. 进入项目目录
cd /Users/shenlan/workspaces/cloud-neutral-toolkit/accounts.svc.plus

# 2. 设置 GCP 项目
export GCP_PROJECT=xzerolab-480008

# 3. 构建并推送 Docker 镜像
make cloudrun-build

# 4. 部署到 Cloud Run
make cloudrun-deploy

# 或者使用 gcloud 命令直接部署
gcloud run deploy accounts-svc-plus \
  --source . \
  --project=xzerolab-480008 \
  --region=asia-northeast1 \
  --platform=managed \
  --allow-unauthenticated
```

**预期结果**:
- Cloud Run 服务更新为最新版本
- `/api/agent-server/v1/users` 和 `/api/agent-server/v1/status` 端点可用
- Agent 能够成功同步配置

### 修复 2: 添加前端 Agent Server 代理路由

**文件**: `console.svc.plus/src/app/api/agent-server/[...segments]/route.ts`

```typescript
export const dynamic = 'force-dynamic'

import type { NextRequest } from 'next/server'

import { createUpstreamProxyHandler } from '@lib/apiProxy'
import { getAccountServiceBaseUrl } from '@server/serviceConfig'

const AGENT_SERVER_PREFIX = '/api/agent-server'

function createHandler() {
  const upstreamBaseUrl = getAccountServiceBaseUrl()
  return createUpstreamProxyHandler({
    upstreamBaseUrl,
    upstreamPathPrefix: AGENT_SERVER_PREFIX,
  })
}

const handler = createHandler()

export function GET(request: NextRequest) {
  return handler(request)
}

export function POST(request: NextRequest) {
  return handler(request)
}

export function PUT(request: NextRequest) {
  return handler(request)
}

export function PATCH(request: NextRequest) {
  return handler(request)
}

export function DELETE(request: NextRequest) {
  return handler(request)
}

export function HEAD(request: NextRequest) {
  return handler(request)
}

export function OPTIONS(request: NextRequest) {
  return handler(request)
}
```

**说明**: 
- 创建代理路由将前端的 `/api/agent-server/*` 请求转发到 `accounts-svc-plus`
- 这个路由主要用于前端调试，agent 服务直接调用 Cloud Run URL

### 修复 3: 增强 Registry 持久化和日志

**文件**: `accounts.svc.plus/internal/agentserver/registry.go`

**变更**:
1. 添加 `logger *slog.Logger` 字段到 `Registry` 结构体
2. 添加 `SetLogger()` 方法
3. 在 `RegisterAgent()` 和 `ReportStatus()` 中添加错误日志

**关键代码**:
```go
// 在 ReportStatus 中添加日志
if err := r.store.UpsertAgent(ctx, dbAgent); err != nil {
    r.logger.Error("failed to persist agent status heartbeat", "agent", a.ID, "err", err)
}

// 在 RegisterAgent 中添加日志
if err := r.store.UpsertAgent(ctx, dbAgent); err != nil {
    r.logger.Error("failed to persist dynamically registered agent", "agent", id, "err", err)
}
```

**文件**: `accounts.svc.plus/cmd/accountsvc/main.go`

```go
if agentRegistry != nil {
    agentRegistry.SetStore(st)
    agentRegistry.SetLogger(logger.With("component", "agent-registry"))
    // ... 其余代码
}
```

### 修复 4: 用户 UUID 变更

**连接数据库**:
```bash
ssh -i ~/.ssh/id_rsa root@postgresql.svc.plus
```

**执行 SQL 事务**:
```sql
BEGIN;

-- 1. 重命名旧用户（避免唯一约束冲突）
UPDATE users 
SET username = username || '_old', 
    email = email || '_old' 
WHERE uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';

-- 2. 创建新用户记录（使用新 UUID）
INSERT INTO users (
    uuid, username, password, email, role, level, groups, permissions, 
    created_at, updated_at, version, origin_node, mfa_totp_secret, 
    mfa_enabled, mfa_secret_issued_at, mfa_confirmed_at, email_verified_at
)
SELECT 
    '18d270a9-533d-4b13-b3f1-e7f55540a9b2', 
    REPLACE(username, '_old', ''), 
    password, 
    REPLACE(email, '_old', ''), 
    role, level, groups, permissions, 
    created_at, updated_at, version, origin_node, mfa_totp_secret, 
    mfa_enabled, mfa_secret_issued_at, mfa_confirmed_at, email_verified_at
FROM users 
WHERE uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';

-- 3. 更新所有外键引用
UPDATE identities 
SET user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2' 
WHERE user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';

UPDATE sessions 
SET user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2' 
WHERE user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';

UPDATE subscriptions 
SET user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2' 
WHERE user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';

-- 4. 删除旧用户记录
DELETE FROM users 
WHERE uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';

COMMIT;
```

**执行命令**:
```bash
docker exec postgresql-svc-plus psql -U postgres -d account -c "
BEGIN;
UPDATE users SET username = username || '_old', email = email || '_old' WHERE uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';
INSERT INTO users (uuid, username, password, email, role, level, groups, permissions, created_at, updated_at, version, origin_node, mfa_totp_secret, mfa_enabled, mfa_secret_issued_at, mfa_confirmed_at, email_verified_at)
SELECT '18d270a9-533d-4b13-b3f1-e7f55540a9b2', REPLACE(username, '_old', ''), password, REPLACE(email, '_old', ''), role, level, groups, permissions, created_at, updated_at, version, origin_node, mfa_totp_secret, mfa_enabled, mfa_secret_issued_at, mfa_confirmed_at, email_verified_at
FROM users WHERE uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';
UPDATE identities SET user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2' WHERE user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';
UPDATE sessions SET user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2' WHERE user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';
UPDATE subscriptions SET user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2' WHERE user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';
DELETE FROM users WHERE uuid = '4b66928e-a81e-4981-bae0-289ddb92439c';
COMMIT;
"
```

**状态**: ✅ 已完成

### 修复 5: 改进前端错误处理

**文件**: `console.svc.plus/src/modules/extensions/builtin/user-center/routes/agent.tsx`

**变更**:
1. 改进 `fetcher` 函数的错误处理
2. 添加错误消息显示

```typescript
async function fetcher(url: string): Promise<VlessNode[]> {
  const res = await fetch(url, { credentials: 'include', cache: 'no-store' })

  const payload = await res.json().catch(() => null)
  if (!res.ok) {
    const message =
      (payload && typeof payload.message === 'string' && payload.message) ||
      (payload && typeof payload.error === 'string' && payload.error) ||
      `Request failed (${res.status})`
    throw new Error(message)
  }

  if (Array.isArray(payload)) {
    return payload as VlessNode[]
  }
  if (payload && Array.isArray((payload as { nodes?: unknown }).nodes)) {
    return (payload as { nodes: VlessNode[] }).nodes
  }

  return []
}

// 在 UI 中显示错误
{error && (
  <div className="rounded-xl border border-[color:var(--color-danger-border)] bg-[var(--color-danger-muted)]/30 px-4 py-3 text-sm text-[var(--color-danger-foreground)]">
    {language === 'zh'
      ? `节点列表加载失败：${error.message}`
      : `Failed to load agent nodes: ${error.message}`}
  </div>
)}
```

**状态**: ✅ 已完成

## 验证方法

### 1. 验证 Cloud Run 部署 ⚠️ **关键验证**

```bash
# 测试 agent API 端点
curl -s -H "Authorization: Bearer uTvryFvAbz6M5sRtmTaSTQY6otLZ95hneBsWqXu+35I=" \
  "https://accounts-svc-plus-266500572462.asia-northeast1.run.app/api/agent-server/v1/users"

# 预期结果: JSON 响应包含用户列表
# {
#   "clients": [...],
#   "total": N,
#   "generated_at": "2026-02-05T07:30:00Z"
# }

# 如果仍返回 404，说明部署未成功
```

### 2. 验证 Agent 同步

```bash
# 在 agent 节点上查看日志
ssh root@hk-xhttp.svc.plus
journalctl -u agent-svc-plus -f

# 预期看到:
# - "xray config synced successfully"
# - 没有 404 错误
```

### 3. 验证 UUID 变更

```bash
# 查询新 UUID
docker exec postgresql-svc-plus psql -U postgres -d account -c "
  SELECT uuid, username, email 
  FROM users 
  WHERE email = 'tester123@example.com';
"

# 预期结果:
#                  uuid                 | username  |         email         
# --------------------------------------+-----------+-----------------------
#  18d270a9-533d-4b13-b3f1-e7f55540a9b2 | tester123 | tester123@example.com
```

### 4. 验证关联数据

```bash
# 检查订阅是否正确关联
docker exec postgresql-svc-plus psql -U postgres -d account -c "
  SELECT user_uuid, external_id, status 
  FROM subscriptions 
  WHERE user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';
"
```

### 5. 验证前端显示

```bash
# 访问 https://www.svc.plus/panel/agent
# 确认页面能够正常加载
# 如果有 401 错误，检查认证 token 传递
```

## 部署步骤

### 步骤 1: 部署 accounts-svc-plus 到 Cloud Run

```bash
# 1. 进入项目目录
cd /Users/shenlan/workspaces/cloud-neutral-toolkit/accounts.svc.plus

# 2. 确认代码已提交
git status
git add .
git commit -m "feat: add agent API routes for /api/agent-server/v1"
git push

# 3. 设置环境变量
export GCP_PROJECT=xzerolab-480008
export GCP_REGION=asia-northeast1

# 4. 构建镜像（如果使用 Makefile）
make cloudrun-build

# 5. 更新 service.yaml 以使用 Secret Manager
# 确保 service.yaml 中 INTERNAL_SERVICE_TOKEN 使用 valueFrom: secretKeyRef 配置

# 6. 部署服务
make cloudrun-deploy

# 或者使用 gcloud 命令
gcloud run deploy accounts-svc-plus \
  --source . \
  --project=$GCP_PROJECT \
  --region=$GCP_REGION \
  --platform=managed \
  --allow-unauthenticated

# 6. 等待部署完成
# 预期输出: Service [accounts-svc-plus] revision [accounts-svc-plus-xxxxx] has been deployed
```

### 步骤 2: 验证部署

```bash
# 测试 API 端点
curl -s -H "Authorization: Bearer uTvryFvAbz6M5sRtmTaSTQY6otLZ95hneBsWqXu+35I=" \
  "https://accounts-svc-plus-266500572462.asia-northeast1.run.app/api/agent-server/v1/users"

# 应该返回 JSON 而不是 404
```

### 步骤 3: 监控 Agent 日志

```bash
# 在 agent 节点上监控日志
ssh root@hk-xhttp.svc.plus
journalctl -u agent-svc-plus -f

# 等待下一次同步周期（5分钟）
# 确认没有 404 错误
```

## 回滚计划

### 如果 Cloud Run 部署导致问题

```bash
# 1. 查看之前的版本
gcloud run revisions list \
  --service=accounts-svc-plus \
  --project=xzerolab-480008 \
  --region=asia-northeast1

# 2. 回滚到之前的版本
gcloud run services update-traffic accounts-svc-plus \
  --to-revisions=PREVIOUS_REVISION=100 \
  --project=xzerolab-480008 \
  --region=asia-northeast1
```

### 如果 UUID 变更导致问题

```sql
-- 反向操作（需要提前备份数据）
BEGIN;

-- 重命名当前用户
UPDATE users 
SET username = username || '_new', 
    email = email || '_new' 
WHERE uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';

-- 恢复旧 UUID
INSERT INTO users (uuid, username, password, email, ...)
SELECT '4b66928e-a81e-4981-bae0-289ddb92439c', 
       REPLACE(username, '_new', ''), 
       ...
FROM users 
WHERE uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';

-- 更新外键
UPDATE identities SET user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c' 
WHERE user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';

UPDATE sessions SET user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c' 
WHERE user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';

UPDATE subscriptions SET user_uuid = '4b66928e-a81e-4981-bae0-289ddb92439c' 
WHERE user_uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';

-- 删除新记录
DELETE FROM users WHERE uuid = '18d270a9-533d-4b13-b3f1-e7f55540a9b2';

COMMIT;
```

## 已知问题

### 1. `/api/agent/nodes` 返回 401 错误
- **现象**: 前端访问 `/api/agent/nodes` 时收到 401 Unauthorized
- **原因**: 认证 token 未正确传递到该端点
- **影响**: 用户无法查看节点列表
- **状态**: 待修复
- **临时方案**: 直接访问后端 API 或使用 admin 账户

### 2. Agent API 路由未部署到生产环境 ⚠️ **阻塞问题**
- **现象**: Cloud Run 服务返回 404
- **原因**: 生产环境运行旧版本代码
- **影响**: Agent 无法同步配置
- **状态**: **需要立即部署**
- **修复**: 执行 `make cloudrun-deploy`

## 相关文档

- [Agent 架构文档](../docs/agent-architecture.md)
- [数据库 Schema](../sql/schema.sql)
- [API 路由配置](../api/api.go)
- [Cloud Run 部署文档](../deploy/gcp/cloud-run/README.md)

## 附录

### Agent 配置示例

**文件**: `/etc/agent/account-agent.yaml`

```yaml
mode: "agent"

log:
  level: info

agent:
  id: "hk-proxy-server"
  controllerUrl: "https://accounts-svc-plus-266500572462.asia-northeast1.run.app"
  apiToken: "uTvryFvAbz6M5sRtmTaSTQY6otLZ95hneBsWqXu+35I="
  httpTimeout: 15s
  statusInterval: 1m
  syncInterval: 5m
  tls:
    insecureSkipVerify: false

xray:
  sync:
    enabled: true
    interval: 5m
    targets:
      - name: "xhttp"
        outputPath: "/usr/local/etc/xray/config.json"
        templatePath: "/usr/local/etc/xray/templates/xray.xhttp.template.json"
        restartCommand:
          - "systemctl"
          - "restart"
          - "xray.service"
      - name: "tcp"
        outputPath: "/usr/local/etc/xray/tcp-config.json"
        templatePath: "/usr/local/etc/xray/templates/xray.tcp.template.json"
        restartCommand:
          - "systemctl"
          - "restart"
          - "xray-tcp.service"
```

### 数据库连接信息

```bash
# SSH 连接
ssh -i ~/.ssh/id_rsa root@postgresql.svc.plus

# Docker 容器名称
postgresql-svc-plus

# 数据库名称
account

# 用户名
postgres

# 密码
见 .env 文件
```

### 相关服务

- **accounts-svc-plus**: Cloud Run 服务，处理认证和用户管理
  - URL: `https://accounts-svc-plus-266500572462.asia-northeast1.run.app`
  - 域名: `https://accounts.svc.plus`
- **console.svc.plus**: 前端控制台
  - URL: `https://www.svc.plus`
- **agent.svc.plus**: Agent 服务节点
  - 节点: `hk-xhttp.svc.plus`, `jp-xhttp.svc.plus`, `us-xhttp.svc.plus`

### 监控和日志

```bash
# 查看 Cloud Run 日志
gcloud run services logs read accounts-svc-plus \
  --project=xzerolab-480008 \
  --region=asia-northeast1 \
  --limit=100

# 查看 Agent 日志
ssh root@hk-xhttp.svc.plus "journalctl -u agent-svc-plus -n 100 --no-pager"

# 查看数据库日志
ssh -i ~/.ssh/id_rsa root@postgresql.svc.plus \
  "docker logs postgresql-svc-plus --tail=100"
```

### 关键 API 端点

```bash
# Agent API 端点（需要 Bearer token）
GET  /api/agent-server/v1/users   # 获取用户列表
POST /api/agent-server/v1/status  # 上报 agent 状态

# 用户 API 端点（需要用户认证）
GET  /api/agent/nodes              # 获取 agent 节点列表

# 认证端点
GET  /api/auth/session             # 获取当前会话
POST /api/auth/login               # 用户登录
```

### 故障排查清单

- [ ] 检查 Cloud Run 服务是否运行最新版本
- [ ] 验证 agent API 端点返回 200 而不是 404
- [ ] 检查 agent 配置文件中的 controller URL 和 token
- [ ] 查看 agent 日志确认没有 404 错误
- [ ] 验证数据库中的 UUID 已正确更新
- [ ] 检查所有外键引用是否指向新 UUID
- [ ] 测试前端页面是否能正常加载
- [ ] 监控 Cloud Run 日志确认没有新错误
