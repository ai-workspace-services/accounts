# API Overview / API 总览

## 中文

### 页面作用

本页描述 `accounts.svc.plus` 当前 HTTP 面的总边界：

- 路由族如何划分。
- 每组路由由哪些 `api/*.go` 文件拥有。
- 认证链路如何叠加。
- 成功与失败响应的公共形状是什么。

参数、返回体字段和逐接口依赖关系请继续阅读：

- [认证与鉴权](auth.md)
- [接口矩阵](endpoints.md)
- [错误约定](errors.md)

### 基础路由族

| 路由族 | 典型路径 | 主要 owner file | 说明 |
| --- | --- | --- | --- |
| 健康与版本 | `GET /healthz` `GET /api/ping` | `api/api.go` | 活性检查与运行时镜像版本信息。 |
| 公共认证入口 | `/api/auth/register` `/api/auth/login` `/api/auth/oauth/*` | `api/api.go` | 注册、登录、邮箱校验、OAuth 跳转、JWT refresh。 |
| 会话保护认证面 | `/api/auth/session` `/api/auth/xworkmate/*` `/api/auth/subscriptions` | `api/api.go` `api/xworkmate.go` `api/config_sync.go` `api/stripe.go` | 以 session token 为主路径的控制面接口。 |
| Auth 作用域下的管理员接口 | `/api/auth/admin/*` | `api/admin_users_metrics.go` `api/admin_users.go` `api/admin_sandbox.go` `api/admin_assume.go` `api/homepage_video.go` | 给 dashboard / BFF 使用的管理员接口。 |
| 公共 `/api/admin/*` 管理面 | `/api/admin/users/metrics` `/api/admin/traffic/*` | `api/admin_users_metrics.go` `api/admin_agents.go` `api/accounting.go` | 与前端约定的管理员根路径。 |
| 内部服务接口 | `/api/internal/*` | `api/internal_public_overview.go` `api/internal_sandbox_guest.go` `api/internal_network_identities.go` `api/accounting.go` | 仅供受信任服务调用，走 internal service token。 |
| Agent 控制接口 | `/api/agent-server/v1/*` | `api/agent_server.go` `api/user_agents.go` | 给 agent 或用户控制台提供节点与客户端视图。 |
| 账户读面 | `/api/account/*` | `api/accounting.go` | 使用量、账单、策略快照读取。 |
| Legacy alias | `/api/agent/nodes` | `api/user_agents.go` | 旧路径别名，规范路径是 `/api/agent-server/v1/nodes`。 |

### 认证模型

#### 1. Session first

当前控制面的主事实来源是 API 进程内 session store：

- 登录、邮箱验证成功、密码重置确认成功、OAuth callback 都会签发 session token。
- token 同时通过响应体字段 `token` 返回，并通过 `xc_session` cookie 下发。
- 多数业务 handler 最终都调用 `handler.lookupSession` / `handler.requireAuthenticatedUser` 做真实鉴权。

#### 2. Optional JWT middleware

`RegisterRoutes` 只在 `tokenService != nil` 时给 `/api/auth` 的保护组和 `/api/admin` 组叠加：

- `auth.TokenService.AuthMiddleware()`
- `auth.RequireActiveUser(h.store)`

因此真实行为是：

- 当 `auth.enable` 关闭时，业务仍按 session 主路径工作。
- 当 `auth.enable` 打开时，部分路由会先经过 JWT middleware，然后在 handler 内继续读取 session。
- 这就是为什么文档统一把当前模式描述为“session-first, JWT-optional”。

#### 3. Internal service token

`/api/internal/*` 统一使用 `auth.InternalAuthMiddleware()`：

- 读取 `Authorization: Bearer <token>`
- 与环境变量 `INTERNAL_SERVICE_TOKEN` 比较
- 失败时直接返回简化错误 JSON

#### 4. Agent token

`/api/agent-server/v1/users` 与 `/api/agent-server/v1/status` 走 `agentserver.Registry.Authenticate`：

- 读取 `Authorization: Bearer <agent-token>`
- 允许共享 token + `X-Agent-ID` / `agentId` 解析具体节点身份
- 认证成功后进入 `Registry` 与 `store.NodeHealthSnapshot` 更新链路

`/api/agent-server/v1/users` 只返回当前允许接入 Xray 的用户配置。存在 billing profile 且月度剩余额度小于等于 0、处于 billing suspension、或被管理员暂停时，账号、订阅、配额、账单和审计记录均永久保留，只暂停向 agent 同步其 Xray 配置；沙箱账号保持豁免。agent 撤下节点本地凭据并重启受影响的 Xray，额度续期或状态恢复后自动重新同步。

`/api/agent-server/v1/users/events` 提供用户配置版本的长连接事件流。agent 以该事件作为主要同步触发器，并保留 10 分钟一次的低频拉取作为断线与漏事件兜底。

`/api/account/usage/summary` 同步返回 `quotaExhausted`、`networkAccessState` 与 `networkAccessReason`，供 Portal 展示实际执行状态，而不是根据余额或旧的 throttle 字段推测。

### 响应约定

| 类别 | 典型形状 | 说明 |
| --- | --- | --- |
| 通用成功 | `{"message": "...", ...}` 或领域对象 JSON | 大多数 handler 直接输出业务对象，不包统一 envelope。 |
| 会话成功 | `{"token": "...", "expiresAt": "...", "user": {...}}` | 登录、邮箱验证、密码重置确认、MFA 完成等都会返回 session 信息。 |
| 通用失败 | `{"error":"code","message":"..."}` | 由 `respondError` 统一组装，是最常见错误形状。 |
| 简化失败 | `{"error":"..."}` | 多见于 agent/internal/simple GORM handlers。 |
| Agent DTO | `agentproto.ClientListResponse`、`204 No Content` | `/users` 返回结构化 DTO，`/status` 成功时无响应体。 |

### 关键 handler owner map

| 文件 | 负责的接口面 |
| --- | --- |
| `api/api.go` | 注册、登录、session、OAuth、MFA、订阅、权限矩阵、`/healthz`、`/api/ping`。 |
| `api/xworkmate.go` | XWorkmate profile、secret locator、Vault-backed secret 读写、tenant bootstrap。 |
| `api/config_sync.go` | `/api/auth/sync/config` 与 `/api/auth/sync/ack`。 |
| `api/stripe.go` | Stripe checkout、portal、webhook。 |
| `api/admin_users_metrics.go` | `/api/admin/*` 路由注册、用户指标权限控制。 |
| `api/admin_users.go` | 创建用户、暂停/恢复、删除、renew uuid、黑名单。 |
| `api/admin_agents.go` | 管理面 agent 状态聚合。 |
| `api/accounting.go` | `/api/account/*`、`/api/admin/traffic/*`、内部策略与心跳。 |
| `api/agent_server.go` | Agent 拉取 client 列表、上报状态。 |
| `api/user_agents.go` | 用户/控制台读取节点列表，含 sandbox 特判。 |
| `api/homepage_video.go` | 公共首页视频与管理员首页视频配置。 |
| `api/admin_sandbox.go` | Sandbox binding 读写。 |
| `api/admin_assume.go` | Root 假扮 sandbox 会话。 |
| `api/internal_*` | 内部 public overview、sandbox guest、network identities。 |

### 当前实现注意点

1. `/api/auth/password/reset` 与 `/api/auth/password/reset/confirm` 被挂在 authProtected 组下，但核心业务逻辑本身仍是邮箱 / token 驱动；启用 JWT middleware 后会多一层前置校验。
2. `/api/auth/sandbox/binding` 既接受正常会话，也接受内部服务 token，因为 Console Guest/Demo 需要在无本地浏览器状态时读取绑定信息。
3. `/api/agent-server/v1/nodes` 没有挂在 token middleware 下，而是在 handler 内自行解析 session 或内部服务身份。
4. `/api/ping` 的 `version`、`commit`、`tag` 来自运行时 `IMAGE` 环境变量解析结果，而不是 Git 本地状态。

## English

### What This Page Covers

This page defines the current HTTP surface of `accounts.svc.plus`:

- how route families are split,
- which `api/*.go` files own them,
- how authentication layers stack,
- and what the shared success / error shapes look like.

For endpoint-level parameters, response fields, and dependency wiring, continue with:

- [Authentication](auth.md)
- [Endpoint Matrix](endpoints.md)
- [Error Conventions](errors.md)

### Route Families

| Route family | Example paths | Main owner files | Purpose |
| --- | --- | --- | --- |
| Health and version | `GET /healthz`, `GET /api/ping` | `api/api.go` | Liveness and runtime image-derived version metadata. |
| Public auth entrypoints | `/api/auth/register`, `/api/auth/login`, `/api/auth/oauth/*` | `api/api.go` | Registration, login, email verification, OAuth redirects, JWT refresh. |
| Session-protected auth surface | `/api/auth/session`, `/api/auth/xworkmate/*`, `/api/auth/subscriptions` | `api/api.go`, `api/xworkmate.go`, `api/config_sync.go`, `api/stripe.go` | Primary control-plane APIs built around session tokens. |
| Auth-scoped admin APIs | `/api/auth/admin/*` | `api/admin_users_metrics.go`, `api/admin_users.go`, `api/admin_sandbox.go`, `api/admin_assume.go`, `api/homepage_video.go` | Admin APIs used by the dashboard / BFF. |
| Public `/api/admin/*` admin root | `/api/admin/users/metrics`, `/api/admin/traffic/*` | `api/admin_users_metrics.go`, `api/admin_agents.go`, `api/accounting.go` | Frontend-facing admin root path. |
| Internal service APIs | `/api/internal/*` | `api/internal_public_overview.go`, `api/internal_sandbox_guest.go`, `api/internal_network_identities.go`, `api/accounting.go` | Trusted service-to-service APIs protected by the internal service token. |
| Agent control APIs | `/api/agent-server/v1/*` | `api/agent_server.go`, `api/user_agents.go` | Node and client views for agents and the control console. |
| Account read models | `/api/account/*` | `api/accounting.go` | Usage, billing, and policy snapshot reads. |
| Legacy alias | `/api/agent/nodes` | `api/user_agents.go` | Backward-compatible alias for `/api/agent-server/v1/nodes`. |

### Authentication Model

#### 1. Session-first

The primary control-plane fact source is the process-local session store:

- login, email verification success, password reset confirmation, and OAuth callback all issue a session token,
- the token is returned in the body as `token` and also set as the `xc_session` cookie,
- most business handlers ultimately call `handler.lookupSession` or `handler.requireAuthenticatedUser`.

#### 2. Optional JWT middleware

`RegisterRoutes` adds these middlewares to the protected `/api/auth` group and the `/api/admin` root only when `tokenService != nil`:

- `auth.TokenService.AuthMiddleware()`
- `auth.RequireActiveUser(h.store)`

So the actual runtime behavior is:

- when `auth.enable` is off, business routes still work through the session-centric path,
- when `auth.enable` is on, some routes first pass JWT middleware and then still load the session in the handler,
- which is why the current system should be described as session-first and JWT-optional.

#### 3. Internal service token

`/api/internal/*` is guarded by `auth.InternalAuthMiddleware()`:

- it reads `Authorization: Bearer <token>`,
- compares it to `INTERNAL_SERVICE_TOKEN`,
- and returns simplified JSON errors on failure.

#### 4. Agent token

`/api/agent-server/v1/users` and `/api/agent-server/v1/status` authenticate through `agentserver.Registry.Authenticate`:

- they read `Authorization: Bearer <agent-token>`,
- support shared tokens plus `X-Agent-ID` / `agentId` to resolve the concrete node,
- and then enter the registry and node-health persistence pipeline.

`/api/agent-server/v1/users` returns only user configurations currently authorized for Xray. When an account with a billing profile has no remaining monthly quota, a billing suspension, or an operator pause, the account, subscription, quota, billing, and audit records are retained permanently; only its Xray configuration sync is paused. The sandbox identity remains exempt. The agent withdraws the node-local credential and restarts the affected Xray instance, then restores it automatically after renewal or reactivation.

`/api/agent-server/v1/users/events` exposes a long-lived user-configuration revision stream. Agents use it as the primary synchronization trigger and retain a low-frequency ten-minute fetch as a disconnect and missed-event fallback.

`/api/account/usage/summary` also returns `quotaExhausted`, `networkAccessState`, and `networkAccessReason` so Portal can display the enforced state without inferring it from balance or the legacy throttle field.

### Response Conventions

| Category | Typical shape | Notes |
| --- | --- | --- |
| Generic success | `{"message": "...", ...}` or a domain object | Most handlers return domain JSON directly instead of a global envelope. |
| Session success | `{"token": "...", "expiresAt": "...", "user": {...}}` | Returned by login, email verification, password reset confirmation, MFA completion, and similar flows. |
| Standard failure | `{"error":"code","message":"..."}` | Produced by `respondError`; this is the most common error envelope. |
| Simplified failure | `{"error":"..."}` | Common in internal / agent / simple GORM-backed handlers. |
| Agent DTOs | `agentproto.ClientListResponse`, `204 No Content` | `/users` returns a typed DTO, while `/status` returns no body on success. |

### Handler Ownership Map

| File | Owned API surface |
| --- | --- |
| `api/api.go` | Registration, login, session, OAuth, MFA, subscriptions, permission matrix, `/healthz`, `/api/ping`. |
| `api/xworkmate.go` | XWorkmate profile, secret locator metadata, Vault-backed secret mutation, tenant bootstrap. |
| `api/config_sync.go` | `/api/auth/sync/config` and `/api/auth/sync/ack`. |
| `api/stripe.go` | Stripe checkout, portal, and webhook handling. |
| `api/admin_users_metrics.go` | `/api/admin/*` route registration plus metrics permission guards. |
| `api/admin_users.go` | User creation, pause/resume, delete, renew UUID, blacklist operations. |
| `api/admin_agents.go` | Aggregated admin agent status view. |
| `api/accounting.go` | `/api/account/*`, `/api/admin/traffic/*`, internal policy reads, node heartbeat ingest. |
| `api/agent_server.go` | Agent client-list reads and status reports. |
| `api/user_agents.go` | User / console node list reads, including sandbox-specific behavior. |
| `api/homepage_video.go` | Public homepage video reads plus admin homepage video settings. |
| `api/admin_sandbox.go` | Sandbox binding reads and writes. |
| `api/admin_assume.go` | Root-only sandbox assume session flow. |
| `api/internal_*` | Internal public overview, sandbox guest, and network identity reads. |

### Important Current Behaviors

1. `/api/auth/password/reset` and `/api/auth/password/reset/confirm` are mounted under the protected auth group even though their business logic is email / reset-token driven; enabling JWT middleware adds an extra precondition.
2. `/api/auth/sandbox/binding` accepts either a normal session or the internal service token because the Console Guest/Demo flow must read binding state without relying on browser-local state.
3. `/api/agent-server/v1/nodes` is intentionally outside token middleware and resolves sessions or trusted internal-service identity inside the handler.
4. `/api/ping` derives `version`, `commit`, and `tag` from the runtime `IMAGE` environment variable, not from local Git state.
