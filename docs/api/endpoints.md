# Endpoint Matrix / 接口矩阵

本页采用单份双语矩阵，避免中文表和英文表在后续维护中漂移。每一行都同时给出中文 / English 描述。

## 1. 健康与版本 / Health And Version

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/healthz` | `api/api.go` | 公开 / Public | 无 / None | `200 {"status":"ok"}` | 无业务依赖 / No business dependency |
| `GET` | `/api/ping` | `api/api.go` | 公开 / Public | 无 / None | `200 {"status","image","tag","commit","version"}` | 运行时 `IMAGE` 环境变量解析 / runtime `IMAGE` parsing |

## 2. 公共认证入口与公共读取 / Public Auth Entry And Public Reads

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `POST` | `/api/auth/register` | `api/api.go` | 公开 / Public | body:`name,email,password,code` | `201 {"message","user"}` | `store.Store`, registration verification cache, subscription upsert |
| `POST` | `/api/auth/register/send` | `api/api.go` | 公开 / Public | body:`email` | `200 {"message":"verification email sent"}` | `store.Store`, `EmailSender`, registration/email verification state |
| `POST` | `/api/auth/register/verify` | `api/api.go` | 公开 / Public | body:`email,code` | `200 {"message","verified"}` or `{"message","token","expiresAt","user"}` | verification caches, `store.Store`, session store |
| `POST` | `/api/auth/login` | `api/api.go` | 公开 / Public | body:`identifier/account/username/email,password,totpCode` | `200 {"message","token","access_token","expiresAt","expires_in","user"}` or MFA challenge payload | `store.Store`, bcrypt, MFA challenge cache, session store |
| `POST` | `/api/auth/mfa/verify` | `api/api.go` | 公开 / Public | body:`mfa_ticket/mfaToken,code/totpCode,method` | `200 {"message","token","access_token","expiresAt","expires_in","user"}` | MFA challenge cache, `store.Store`, session store |
| `POST` | `/api/auth/token/exchange` | `api/api.go` | 公开 / Public | body:`exchange_code` | `200 {"token","access_token","token_type","expiresAt","expires_in","user"}` | OAuth exchange-code cache, session store, `store.Store` |
| `GET` | `/api/auth/oauth/login/:provider` | `api/api.go` | 公开 / Public | path:`provider` | `307` redirect to provider auth URL | configured `auth.OAuthProvider` |
| `GET` | `/api/auth/oauth/callback/:provider` | `api/api.go` | 公开 / Public | path:`provider`; query:`code,state?` | `307` redirect to frontend `/login?exchange_code=...` | `OAuthProvider`, `store.Store`, identity binding, session store |
| `POST` | `/api/auth/token/refresh` | `api/api.go` | 公开 / Public | body:`refresh_token` | `200 {"access_token","token_type","expires_in"}` | optional `auth.TokenService` |
| `POST` | `/api/auth/refresh` | `api/api.go` | 公开 / Public | body:`refresh_token` | same as `/token/refresh` | optional `auth.TokenService` |
| `GET` | `/api/auth/mfa/status` | `api/api.go` | 公开入口；可用 session 或 MFA token / public entry using session or MFA token | query:`token,identifier,email`; header:`X-MFA-Token`; `Authorization` optional | `200 {"enabled","mfa","user"}` or `{"mfa_enabled":false}` | MFA challenge cache, session store, `store.Store` |
| `GET` | `/api/auth/sync/config` | `api/config_sync.go` | handler 内要求 session / session enforced in handler | query:`since_version` | `200 {"schema_version","changed","version","updated_at","profiles","nodes","rendered_json","dns","meta","digest","warnings"}` | session store, `store.Store`, agent status reader, xray renderer |
| `POST` | `/api/auth/sync/ack` | `api/config_sync.go` | handler 内要求 session / session enforced in handler | body:`version,device_id,applied_at` | `200 {"acked","version","device_id","user_id","received_at"}` | session store, `store.Store` |
| `GET` | `/api/auth/homepage-video` | `api/homepage_video.go` | 公开 / Public | host/header context only | `200 {"resolved":{"videoUrl","posterUrl",...}}` | GORM-backed homepage video settings |
| `GET` | `/api/auth/sandbox/binding` | `api/sandbox_binding_public.go` | session 或 internal service token / session or internal service token | 无 / None | `200 {"address","updatedAt"}` | session store or internal token, GORM DB |

## 3. 会话主路径接口 / Session-Centric Protected Routes

说明 / Note:

- 这些接口挂在 `authProtected` 组下。
- 当 `tokenService` 启用时，会先经过 JWT middleware 与 `RequireActiveUser`。
- 但多数 handler 仍继续读取 session，所以文档中的 auth 描述仍按“session-first”记录。

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/auth/session` | `api/api.go` | session；启用时再叠加 JWT / session plus optional JWT middleware | 无 / None | `200 {"user":...}` | session store, `store.Store`, XWorkmate access builder |
| `DELETE` | `/api/auth/session` | `api/api.go` | session / Session | 无 / None | `204 No Content` | session store |
| `GET` | `/api/auth/xworkmate/profile` | `api/xworkmate.go` | session / Session | host-derived tenant context | `200 {"edition","tenant","membershipRole","profileScope","canEditIntegrations","canManageTenant","profile","tokenConfigured"}` | `store.Store`, tenant resolution |
| `GET` | `/api/auth/xworkmate/profile/sync` | `api/xworkmate.go` | session / Session | host-derived tenant context | `200 {"BRIDGE_SERVER_URL","BRIDGE_AUTH_TOKEN"}` | `store.Store`, tenant resolution, vault/profile lookup |
| `PUT` | `/api/auth/xworkmate/profile` | `api/xworkmate.go` | session + tenant permission / session plus tenant permission | body:`profile` or raw profile payload | same shape as profile GET | `store.Store`, tenant membership checks |
| `GET` | `/api/auth/xworkmate/secrets` | `api/xworkmate.go` | session / Session | host-derived tenant context | `200 {"tenant","membershipRole","profileScope","canEditIntegrations","vaultBackendEnabled","tokenConfigured","secrets":[]}` | `store.Store`, Vault service status, locator metadata |
| `PUT` | `/api/auth/xworkmate/secrets/:target` | `api/xworkmate.go` | session + tenant permission / session plus tenant permission | path:`target`; body:`value` | `200 {"secret", "profileScope", "tokenConfigured"}` | `XWorkmateVaultService`, `store.Store` |
| `DELETE` | `/api/auth/xworkmate/secrets/:target` | `api/xworkmate.go` | session + tenant permission / session plus tenant permission | path:`target` | `200 {"secret", "profileScope", "tokenConfigured"}` | `XWorkmateVaultService`, `store.Store` |
| `POST` | `/api/auth/mfa/totp/provision` | `api/api.go` | session 或 mfa token / session or MFA token | body:`token,issuer,account` | `200 {"secret","otpauth_url","issuer","account","mfaToken","mfa","user"}` | MFA challenge cache, session store, `store.Store` |
| `POST` | `/api/auth/mfa/totp/verify` | `api/api.go` | MFA token / MFA token | body:`token,code` | `200 {"message","token","expiresAt","user"}` | MFA challenge cache, TOTP verify, session store |
| `POST` | `/api/auth/mfa/disable` | `api/api.go` | session / Session | header/query token | `200 {"message":"mfa_disabled","user"}` | session store, `store.Store` |
| `POST` | `/api/auth/password/reset` | `api/api.go` | 挂在 protected 组；handler 按邮箱运行 / mounted in protected group; handler logic is email-driven | body:`email` | `202 {"message":"if the account exists a reset email will be sent"}` | `store.Store`, password-reset cache, `EmailSender` |
| `POST` | `/api/auth/password/reset/confirm` | `api/api.go` | 挂在 protected 组；handler 按 reset token 运行 / mounted in protected group; handler logic is reset-token-driven | body:`token,password` | `200 {"message","token","expiresAt","user"}` | password-reset cache, bcrypt, session store, `store.Store` |
| `GET` | `/api/auth/subscriptions` | `api/api.go` | session / Session | 无 / None | `200 {"subscriptions":[...]}` | session store, `store.Store` |
| `POST` | `/api/auth/subscriptions` | `api/api.go` | session / Session | body:`externalId,provider,paymentMethod,paymentQr,kind,planId,status,meta` | `200 {"subscription":...}` | session store, `store.Store` |
| `POST` | `/api/auth/subscriptions/cancel` | `api/api.go` | session / Session | body:`externalId` | `200 {"subscription":...}` | session store, `store.Store`, optional Stripe cancel |
| `POST` | `/api/auth/stripe/checkout` | `api/stripe.go` | session / Session | body:`planId,stripePriceId,mode,productSlug,sourcePath` | `200 {"url","id"}` | session store, `store.Store`, Stripe client |
| `GET` | `/api/auth/stripe/pay` | `api/stripe.go` | session / Session | no body | `303` to configured Stripe Payment Link | session store, Stripe Payment Link config |
| `POST` | `/api/auth/stripe/portal` | `api/stripe.go` | session / Session | body:`returnPath` | `200 {"url","id"}` | session store, `store.Store`, Stripe client |
| `POST` | `/api/auth/config/sync` | `api/config_sync.go` | session / Session | body ignored; optional query:`since_version` | same as `GET /api/auth/sync/config` | session store, `store.Store`, agent status reader, xray renderer |
| `GET` | `/api/auth/admin/settings` | `api/api.go` | admin / operator permission on session | 无 / None | `200 {"version","matrix"}` | session store, `service.GetAdminSettings`, GORM DB |
| `POST` | `/api/auth/admin/settings` | `api/api.go` | admin permission on session | body:`version,matrix` | `200 {"version","matrix"}`; optimistic conflict returns `409 {"error","version","matrix"}` | session store, `service.SaveAdminSettings`, GORM DB |
| `GET` | `/api/auth/admin/homepage-video` | `api/homepage_video.go` | admin permission on session | 无 / None | `200 {"defaultEntry","overrides"}` | session store, homepage video GORM service |
| `PUT` | `/api/auth/admin/homepage-video` | `api/homepage_video.go` | admin permission on session | body:`defaultEntry,overrides` | `200 {"defaultEntry","overrides"}` | session store, homepage video GORM service |
| `GET` | `/api/auth/users` | `api/api.go` | admin read permission on session / admin read permission on session | 无 / None | `200 [sanitizeUser,...]` | session store, `store.Store`, admin permission guard |

## 4. Auth 作用域管理员接口 / Auth-Scoped Admin Operations

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/auth/admin/users/metrics` | `api/admin_users_metrics.go` | admin / operator session | query-driven filters handled by provider | `200` metrics overview payload | session store, `service.UserMetricsProvider`, admin permission guard |
| `POST` | `/api/auth/admin/users` | `api/admin_users.go` | admin session | body:`email,uuid,groups` | `201 {"message":"user_created","user":...}` | session store, `store.Store` |
| `POST` | `/api/auth/admin/users/:userId/role` | `api/api.go` | admin session | path:`userId`; body role payload | `200 {"message","user"}` | session store, `store.Store`, role validation |
| `DELETE` | `/api/auth/admin/users/:userId/role` | `api/api.go` | admin session | path:`userId` | `200 {"message","user"}` | session store, `store.Store` |
| `POST` | `/api/auth/admin/users/:userId/pause` | `api/admin_users.go` | admin session | path:`userId` | `200 {"message":"user_paused"}` | session store, `store.Store` |
| `POST` | `/api/auth/admin/users/:userId/resume` | `api/admin_users.go` | admin session | path:`userId` | `200 {"message":"user_resumed"}` | session store, `store.Store` |
| `DELETE` | `/api/auth/admin/users/:userId` | `api/admin_users.go` | admin session | path:`userId` | `200 {"message":"user_deleted"}` | session store, `store.Store` |
| `POST` | `/api/auth/admin/users/:userId/renew-uuid` | `api/admin_users.go` | admin session | path:`userId`; body:`expires_in_days,expires_at` | `200 {"message","proxy_uuid","expires_at"}`; UUID remains equal to `users.uuid` | session store, `store.Store` |
| `POST` | `/api/auth/admin/tenants/bootstrap` | `api/xworkmate.go` | root session / Root session | body:`name,adminUserId,adminEmail` | `201 {"tenant":{"id","name","edition","domain"},"member":{"id","email","role"}}` | session store, tenant/store models |
| `GET` | `/api/auth/admin/blacklist` | `api/admin_users.go` | admin session | 无 / None | `200 {"blacklist":[...]}` | session store, `store.Store` |
| `POST` | `/api/auth/admin/blacklist` | `api/admin_users.go` | admin session | body blacklist entry | `200 {"message":...}` | session store, `store.Store` |
| `DELETE` | `/api/auth/admin/blacklist/:email` | `api/admin_users.go` | admin session | path:`email` | `200 {"message":...}` | session store, `store.Store` |
| `GET` | `/api/auth/admin/sandbox/binding` | `api/admin_sandbox.go` | root/admin session depending on guard | 无 / None | `200 {"address","updatedAt"}` | session store, GORM DB |
| `POST` | `/api/auth/admin/sandbox/bind` | `api/admin_sandbox.go` | root/admin session depending on guard | body:`address` | `200 {"message","address"}` | session store, GORM DB, `agentserver.Registry` sandbox set |
| `POST` | `/api/auth/admin/assume` | `api/admin_assume.go` | root session / Root session | body:`email` | `200 {"ok":true,"assumed","token","expiresAt"}` | session store, `store.Store`, sandbox UUID rotation |
| `POST` | `/api/auth/admin/assume/revert` | `api/admin_assume.go` | root session / Root session | 无 / None | `200 {"ok":true}` | session store |
| `GET` | `/api/auth/admin/assume/status` | `api/admin_assume.go` | root session / Root session | 无 / None | `200 {"isAssuming","target"}` | session store |

## 5. 公共 `/api/admin/*` 管理面 / Public `/api/admin/*` Admin Root

说明 / Note:

- 这组路由由 `registerAdminRoutes` 注册。
- 语义与部分 `/api/auth/admin/*` 路由共享同一 handler。
- 若启用了 token service，会先经过 JWT middleware 与 `RequireActiveUser`。

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/admin/users/metrics` | `api/admin_users_metrics.go` | admin / operator session | query filters | `200` metrics overview payload | session store, `service.UserMetricsProvider` |
| `GET` | `/api/admin/agents/status` | `api/admin_agents.go` | admin session | 无 / None | `200 {"agents":[{id,name,groups,healthy,message,users,syncRevision,updatedAt,xray{...}}]}` | `agentserver.Registry`, `store.Store` |
| `GET` | `/api/admin/traffic/nodes` | `api/accounting.go` | admin session | 无 / None | `200 {"nodes":[NodeHealthSnapshot...]}` | `store.Store` |
| `GET` | `/api/admin/traffic/accounts/:uuid` | `api/accounting.go` | admin session | path:`uuid` | `200 {"accountUuid","buckets","ledger","policy","quotaState","billingProfile"}` | `store.Store` |
| `GET` | `/api/admin/collector/status` | `api/accounting.go` | admin session | 无 / None | `200 {"checkpoints","recentBuckets"}` | `store.Store` |
| `GET` | `/api/admin/scheduler/status` | `api/accounting.go` | admin session | 无 / None | `200 {"decisions":[...]}` | `store.Store` |
| `POST` | `/api/admin/users` | `api/admin_users.go` | admin session | body:`email,uuid,groups` | `201 {"message","user"}` | session store, `store.Store` |
| `POST` | `/api/admin/users/:userId/pause` | `api/admin_users.go` | admin session | path:`userId` | `200 {"message":"user_paused"}` | session store, `store.Store` |
| `POST` | `/api/admin/users/:userId/resume` | `api/admin_users.go` | admin session | path:`userId` | `200 {"message":"user_resumed"}` | session store, `store.Store` |
| `DELETE` | `/api/admin/users/:userId` | `api/admin_users.go` | admin session | path:`userId` | `200 {"message":"user_deleted"}` | session store, `store.Store` |
| `POST` | `/api/admin/users/:userId/renew-uuid` | `api/admin_users.go` | admin session | path:`userId`; body:`expires_in_days,expires_at` | `200 {"message","proxy_uuid","expires_at"}`; UUID remains equal to `users.uuid` | session store, `store.Store` |
| `GET` | `/api/admin/blacklist` | `api/admin_users.go` | admin session | 无 / None | `200 {"blacklist":[...]}` | session store, `store.Store` |
| `POST` | `/api/admin/blacklist` | `api/admin_users.go` | admin session | body blacklist entry | `200 {"message":...}` | session store, `store.Store` |
| `DELETE` | `/api/admin/blacklist/:email` | `api/admin_users.go` | admin session | path:`email` | `200 {"message":...}` | session store, `store.Store` |
| `GET` | `/api/admin/sandbox/binding` | `api/admin_sandbox.go` | admin/root session | 无 / None | `200 {"address","updatedAt"}` | session store, GORM DB |
| `POST` | `/api/admin/sandbox/bind` | `api/admin_sandbox.go` | admin/root session | body:`address` | `200 {"message","address"}` | session store, GORM DB, `agentserver.Registry` |

## 6. Webhook 与内部服务接口 / Webhook And Internal Service APIs

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `POST` | `/api/billing/stripe/webhook` | `api/stripe.go` | Stripe webhook signature / Stripe webhook signature | raw Stripe event body | `200 {"received":true}` | Stripe webhook verifier, `store.Store` |
| `GET` | `/api/internal/public-overview` | `api/internal_public_overview.go` | internal service token | 无 / None | `200 {"registeredUsers","updatedAt"}` | `store.Store` |
| `GET` | `/api/internal/sandbox/guest` | `api/internal_sandbox_guest.go` | internal service token | 无 / None | `200 {"email","proxyUuid","proxyUuidExpiresAt"}` | `store.Store`, sandbox UUID rotation |
| `GET` | `/api/internal/network/identities` | `api/internal_network_identities.go` | internal service token | 无 / None | `200 {"generatedAt","identities":[{uuid,email,accountUuid}]}` | `store.Store` |
| `GET` | `/api/internal/policy/:accountUUID` | `api/accounting.go` | internal service token | path:`accountUUID` | `200` account policy snapshot | `store.Store` |
| `POST` | `/api/internal/nodes/heartbeat` | `api/accounting.go` | internal service token | body:`nodeId,region,lineCode,pricingGroup,statsEnabled,xrayRevision,healthy,latencyMs,errorRate,activeConnections,healthScore,sampledAt` | `204 No Content` | `store.Store` node health persistence |

## 7. Agent 与节点发现接口 / Agent And Node Discovery APIs

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/agent-server/v1/nodes` | `api/user_agents.go` | session，或 trusted internal service for sandbox / session or trusted internal service | session token or internal token; no body | `200 []VlessNode` | session store, `store.Store`, sandbox UUID rotation, agent status reader |
| `GET` | `/api/agent-server/v1/users` | `api/agent_server.go` | agent token / Agent token | header:`Authorization`; optional `X-Agent-ID`; query:`agentId` | `200 agentproto.ClientListResponse{clients,total,generatedAt}` | `agentserver.Registry`, `store.Store`, `xrayconfig.Client` projection |
| `POST` | `/api/agent-server/v1/status` | `api/agent_server.go` | agent token / Agent token | body:`agentproto.StatusReport` | `204 No Content` | `agentserver.Registry`, `store.NodeHealthSnapshot` upsert |
| `GET` | `/api/agent/nodes` | `api/user_agents.go` | legacy alias; same auth as canonical route / legacy alias; same auth as canonical route | same as `/api/agent-server/v1/nodes` | same as canonical route | same as canonical route |

## 8. 账户读面接口 / Account Read Models

| 方法 / Method | 路径 / Path | Owner file | 认证 / Auth | 请求参数 / Request | 成功返回 / Success | 主要依赖 / Main dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `GET` | `/api/account/usage/summary` | `api/accounting.go` | session / Session | no body; account resolved from current session user | `200 {"accountUuid","totalBytes","uplinkBytes","downlinkBytes","sourceOfTruth","currentBalance","remainingIncludedQuota","suspendState","throttleState","arrears","lastBucketAt","syncDelaySeconds","billingProfile"}` | session store, `store.Store` usage summary readers |
| `GET` | `/api/account/usage/buckets` | `api/accounting.go` | session / Session | query:`start,end`; account resolved from current session user | `200 {"accountUuid","buckets","sourceOfTruth"}` | session store, `store.Store` traffic buckets |
| `GET` | `/api/account/billing/summary` | `api/accounting.go` | session / Session | no body; account resolved from current session user | `200 {"accountUuid","quotaState","billingProfile","ledger","sourceOfTruth"}` | session store, `store.Store` quota/billing readers |
| `GET` | `/api/account/policy` | `api/accounting.go` | session / Session | no body; account resolved from current session user | `200 {"accountUuid","policyVersion","authState","rateProfile","connProfile","eligibleNodeGroups","preferredStrategy","degradeMode","expiresAt"}` | session store, `store.Store` policy snapshot reader |

## 9. 备注 / Notes

1. `/api/auth/*` 与 `/api/admin/*` 中很多接口最终共享同一 handler；差别主要在挂载位置和前端调用习惯，而不是业务语义。
2. `/api/auth/password/reset*` 当前真实挂载位置会在启用 JWT middleware 时引入额外前置条件；这是“挂载策略”和“handler 语义”之间需要特别注意的实现细节。
3. `account` 读接口与 `admin/traffic` 读接口都直接来自 `store.Store` 的事实表读取，不经过单独的 service 层聚合。
4. 如需错误码、失败 envelope 和来源层说明，请继续查看 [errors.md](errors.md)。
