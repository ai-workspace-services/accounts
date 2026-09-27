# Authentication And Authorization / 认证与鉴权

## 中文

### 当前认证模型

`accounts.svc.plus` 不是“纯 JWT API”，而是：

- 以 session token 为主控制面事实来源。
- 以 `xc_session` cookie 和 `Authorization: Bearer <session-token>` 为主调用方式。
- 在 `auth.enable` 打开时，为部分路由叠加 JWT middleware。
- 对 `/api/internal/*` 使用 internal service token。
- 对 `/api/agent-server/v1/users|status` 使用 agent token。

这意味着“JWT 已启用”并不等于“业务只靠 JWT 运行”；很多 handler 仍会继续读取 session。

### 认证方式总表

| 方式 | 典型接口 | 传入位置 | 成功后得到什么 |
| --- | --- | --- | --- |
| Session token | `/api/auth/login` `/api/auth/session` `/api/auth/xworkmate/*` | `Authorization` 或 `xc_session` cookie | 当前用户上下文、管理员权限、XWorkmate profile 读写能力。 |
| JWT refresh | `POST /api/auth/token/refresh` | JSON body `refresh_token` | 新 `access_token`。 |
| OAuth exchange code | `POST /api/auth/token/exchange` | JSON body `exchange_code` | 真实 session token，字段名同时以 `token` / `access_token` 返回。 |
| Internal service token | `/api/internal/*` | `Authorization: Bearer <token>` | 受信任服务读接口。 |
| Agent token | `/api/agent-server/v1/users` `/api/agent-server/v1/status` | `Authorization: Bearer <token>` | Agent 身份、client 列表拉取、状态上报。 |

### Session issuance paths

以下路径会签发新的 session token：

| 入口 | 请求字段 | 成功返回 | 备注 |
| --- | --- | --- | --- |
| `POST /api/auth/login` | `identifier/account/username/email` + `password`，或邮箱 + `totpCode` | `message`、`token`、`access_token`、`expiresAt`、`expires_in`、`user` | 如果用户已启用 MFA 且未提交验证码，则不会发 session，而是先返回 MFA challenge。 |
| `POST /api/auth/register/verify` | `email`、`code` | `message`、`token`、`expiresAt`、`user` 或 `verified=true` | 已创建用户走“验证邮箱并自动登录”；预注册验证码走“仅标记 verified”。 |
| `POST /api/auth/password/reset/confirm` | `token`、`password` | `message`、`token`、`expiresAt`、`user` | 重置密码成功后直接进入新会话。 |
| `POST /api/auth/mfa/verify` | `mfa_ticket/mfaToken`、`code/totpCode` | `message`、`token`、`access_token`、`expiresAt`、`expires_in`、`user` | 登录后补做 MFA 的完成步骤。 |
| `POST /api/auth/mfa/totp/verify` | `token`、`code` | `message`、`token`、`expiresAt`、`user` | 首次启用 TOTP 成功后直接签发 session。 |
| `GET /api/auth/oauth/callback/:provider` | query `code` | `307` redirect 到前端 `/login?exchange_code=...` | callback 自身不直接输出 JSON，而是先创建 session，再下发一次性 exchange code。 |

### 注册与邮箱验证

#### `POST /api/auth/register/send`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `email` |
| 成功返回 | `200 {"message":"verification email sent"}` |
| 前置条件 | 邮箱格式合法；不在 blacklist；若邮箱已存在且未验证，则继续发送验证邮件。 |
| 失败返回 | `invalid_request`、`invalid_email`、`email_blacklisted`、`smtp_timeout`、`verification_failed`、`email_already_exists`。 |

#### `POST /api/auth/register`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `name`、`email`、`password`、`code` |
| 成功返回 | `201 {"message":"registration successful","user":...}` |
| 前置条件 | `name` 非空；邮箱合法；密码长度至少 8；若启用邮箱验证则 `code` 必须存在且匹配。 |
| 关键副作用 | 创建 `store.User`；自动 upsert 7 天 trial `store.Subscription`。 |
| 失败返回 | `name_required`、`missing_credentials`、`invalid_email`、`password_too_short`、`verification_required`、`invalid_code`、`email_already_exists`、`name_already_exists`。 |

#### `POST /api/auth/register/verify`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `email`、`code` |
| 成功返回 A | `200 {"message":"email verified","token":"...","expiresAt":"...","user":...}` |
| 成功返回 B | `200 {"message":"verification successful","verified":true}` |
| 分支解释 | 若命中已创建用户的 email verification，接口会把用户标记为已验证并直接签发 session；若命中注册前验证码，只做“验证码通过”标记。 |
| 失败返回 | `token_in_query`、`invalid_request`、`invalid_code`、`verification_failed`、`session_creation_failed`。 |

### 登录、MFA challenge 与会话读取

#### `POST /api/auth/login`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `identifier`、`account`、`username`、`email`、`password`、`totpCode` |
| 标准成功返回 | `message`、`token`、`access_token`、`expiresAt`、`expires_in`、`mfaRequired=false`、`user` |
| MFA challenge 返回 | `message="mfa required"`、`mfaRequired=true`、`mfaMethod="totp"`、`mfaTicket`、`mfa_ticket`、兼容字段 `mfaToken` |
| 真实规则 | 先按 `identifier -> account -> username -> email` 解析登录标识。若用户已启用 MFA 但未提交 `totpCode`，登录不发 session，只发 challenge。 |
| 失败返回 | `credentials_in_query`、`invalid_request`、`missing_credentials`、`user_not_found`、`invalid_credentials`、`password_required`、`email_not_verified`、`sandbox_no_login`、`mfa_challenge_creation_failed`。 |

#### `POST /api/auth/mfa/verify`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `mfa_ticket`、`mfaToken`、`code`、`totpCode`、`method` |
| 成功返回 | `message`、`token`、`access_token`、`expiresAt`、`expires_in`、`user` |
| 前置条件 | `method` 仅支持 `totp`；ticket 必须存在且未过期；用户已启用 MFA。 |
| 失败返回 | `mfa_ticket_required`、`mfa_code_required`、`unsupported_mfa_method`、`invalid_mfa_ticket`、`mfa_not_enabled`、`invalid_mfa_code`。 |

#### `GET /api/auth/session`

| 项 | 内容 |
| --- | --- |
| 认证 | 需要有效 session；若启用 token middleware，还会先经过 JWT + active-user 检查。 |
| 成功返回 | `{"user": ...}`，其中可能附带 `tenantId`、`tenants`、XWorkmate access 视图。 |
| 失败返回 | `session_token_required`、`invalid_session`、`session_user_lookup_failed`、`account_suspended`。 |

#### `DELETE /api/auth/session`

| 项 | 内容 |
| --- | --- |
| 认证 | 需要有效 session。 |
| 成功返回 | `204 No Content` |
| 副作用 | 删除进程内 session，并清空 cookie。 |

### TOTP 启用、查询与关闭

#### `POST /api/auth/mfa/totp/provision`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `token`、`issuer`、`account` |
| 成功返回 | `secret`、`otpauth_url`、`issuer`、`account`、`mfaToken`、`mfa`、`user` |
| 入口模式 | 可使用已有 `mfa token`，也可使用当前 session 自动创建新的 MFA challenge。 |
| 失败返回 | `invalid_request`、`invalid_mfa_token`、`mfa_token_required`、`invalid_session`、`mfa_already_enabled`、`read_only_account`、`mfa_secret_generation_failed`、`mfa_setup_failed`。 |

#### `POST /api/auth/mfa/totp/verify`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `token`、`code` |
| 成功返回 | `{"message":"mfa_verified","token":"...","expiresAt":"...","user":...}` |
| 特殊失败 | 多次错误会进入 `429 {"error":"mfa_challenge_locked","retryAt":"...","mfaToken":"..."}`。 |
| 常见失败 | `mfa_token_required`、`invalid_mfa_token`、`mfa_secret_missing`、`mfa_code_required`、`invalid_mfa_code`、`mfa_update_failed`。 |

#### `GET /api/auth/mfa/status`

| 项 | 内容 |
| --- | --- |
| 输入来源 | query `token` / `identifier` / `email`，header `X-MFA-Token`，或 `Authorization` session token。 |
| 成功返回 | `enabled`、`mfa`、`user`。若按 identifier 查询且用户不存在，返回 `200 {"mfa_enabled":false}`。 |
| 失败返回 | `mfa_status_failed`、`mfa_token_required`。 |

#### `POST /api/auth/mfa/disable`

| 项 | 内容 |
| --- | --- |
| 认证 | 当前 session token，优先从 `Authorization` 读取，也接受 query `token`。 |
| 成功返回 | `{"message":"mfa_disabled","user":...}` |
| 失败返回 | `session_token_required`、`invalid_session`、`mfa_disable_failed`、`mfa_not_enabled`、`read_only_account`。 |

### OAuth 与 exchange code

#### `GET /api/auth/oauth/login/:provider`

| 项 | 内容 |
| --- | --- |
| 路径参数 | `provider`，当前实现支持 `github` 与 `google`。 |
| 成功返回 | `307 Temporary Redirect` 到 provider authorization URL。 |
| 失败返回 | `provider_not_found`。 |

#### `GET /api/auth/oauth/callback/:provider`

| 项 | 内容 |
| --- | --- |
| 输入 | query `code`，可选由 `state` 带回前端 URL。 |
| 成功行为 | 交换 provider token，拉取 profile，创建或复用用户，确保 `store.Identity` 绑定，然后签发 session + 一次性 exchange code，最后 `307` 跳转到前端 `/login?exchange_code=...`。 |
| 失败返回 | `provider_not_found`、`code_missing`、`oauth_exchange_failed`、`fetch_profile_failed`、`email_missing`、`email_not_verified`、`store_error`、`user_creation_failed`、`session_creation_failed`、`exchange_code_creation_failed`。 |

#### `POST /api/auth/token/exchange`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `exchange_code` |
| 成功返回 | `token`、`access_token`、`token_type`、`expiresAt`、`expires_in`、`user` |
| 语义 | 不是按 user claims 自签 token，而是把 callback 阶段暂存的一次性 code 换回真实 session。 |
| 失败返回 | `invalid_request`、`invalid_exchange_code`、`session_user_lookup_failed`。 |

### JWT refresh

#### `POST /api/auth/token/refresh`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `refresh_token` |
| 成功返回 | `access_token`、`token_type="Bearer"`、`expires_in` |
| 前置条件 | `h.tokenService != nil`；refresh token 合法且未过期。 |
| 失败返回 | `token_service_unavailable`、`invalid_request`、`invalid_refresh_token`。 |

#### `POST /api/auth/refresh`

别名路由，行为与 `POST /api/auth/token/refresh` 完全一致。

### 密码重置

#### `POST /api/auth/password/forgot` 与 `POST /api/auth/password/forgot/send-code`

| 项 | 内容 |
| --- | --- |
| 认证 | 公开入口；无需已有 session。 |
| 请求字段 | token 流程：`email`；验证码流程：`email`。 |
| 成功返回 | `202`，不泄露账号是否存在。 |
| 持久化 | `password_recovery_challenges` 保存 token SHA-256 或验证码 bcrypt 哈希、用户及邮箱快照、过期时间和尝试/锁定状态；原始凭据不入库。 |

#### `POST /api/auth/password/forgot/confirm` 与 `POST /api/auth/password/forgot/confirm-code`

| 项 | 内容 |
| --- | --- |
| 认证 | 公开入口；依赖有效的一次性恢复挑战。 |
| 请求字段 | token：`token,password`；验证码：`email,code,password`。 |
| 成功行为 | 单事务消费挑战、仅更新用户密码哈希并撤销目标用户现有 sessions。token 流程随后签发新的登录 session。 |
| 保护 | 挑战过期、重复使用或达到验证码错误锁定时均拒绝。 |

#### `POST /api/auth/password/reset`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `email` |
| 成功返回 | `202 {"message":"if the account exists a reset email will be sent"}` |
| 实现说明 | handler 本身按邮箱驱动；但该路由被挂在 authProtected 组下，因此启用 JWT middleware 后会多一层前置认证。 |
| 失败返回 | `email_in_query`、`invalid_request`、`email_required`、`password_reset_failed`、`read_only_account`。 |

#### `POST /api/auth/password/reset/confirm`

| 项 | 内容 |
| --- | --- |
| 请求字段 | `token`、`password` |
| 成功返回 | `message`、`token`、`expiresAt`、`user` |
| 前置条件 | password 长度至少 8；reset token 有效；若用户是 demo/read-only account 则拒绝。 |
| 副作用 | 成功后撤销旧 session，再创建新 session；只改密码哈希，不触碰订阅、支付、用量或其他用户属性。 |
| 失败返回 | `credentials_in_query`、`invalid_request`、`password_too_short`、`invalid_token`、`password_reset_failed`、`read_only_account`、`session_creation_failed`。 |

### XWorkmate profile 与 Vault-backed secrets

#### `GET /api/auth/xworkmate/profile`

| 项 | 内容 |
| --- | --- |
| 成功返回 | `edition`、`tenant`、`membershipRole`、`profileScope`、`canEditIntegrations`、`canManageTenant`、`profile`、`tokenConfigured` |
| profile 字段 | `BRIDGE_SERVER_URL`、`bridgeServerOrigin`、`vaultUrl`、`vaultNamespace`、`vaultSecretPath`、`vaultSecretKey`、`secretLocators`、`apisixUrl` |
| 失败返回 | `tenant_membership_required`、`tenant_not_found`、`xworkmate_context_failed`、`xworkmate_profile_read_failed`。 |

#### `GET /api/auth/xworkmate/profile/sync`

| 项 | 内容 |
| --- | --- |
| 成功返回 | `BRIDGE_SERVER_URL`、`BRIDGE_AUTH_TOKEN` |
| 语义 | 给 bridge / desktop sync 使用的精简同步视图，不返回完整 profile。 |
| 失败返回 | `tenant_membership_required`、`tenant_not_found`、`bridge_server_url_unavailable`、`bridge_auth_token_unavailable`。 |

#### `PUT /api/auth/xworkmate/profile`

| 项 | 内容 |
| --- | --- |
| 请求字段 | 允许直接传 profile payload，或 `{ "profile": {...} }` |
| 禁止字段 | raw token/password/api key 不允许持久化；命中时返回 `token_persistence_forbidden`。 |
| 成功返回 | 与 `GET /xworkmate/profile` 同形。 |
| 失败返回 | `xworkmate_profile_forbidden`、`read_only_account`、`invalid_request`、`token_persistence_forbidden`、`xworkmate_profile_write_failed`。 |

#### `GET /api/auth/xworkmate/secrets`

| 项 | 内容 |
| --- | --- |
| 成功返回 | `tenant`、`membershipRole`、`profileScope`、`canEditIntegrations`、`vaultBackendEnabled`、`tokenConfigured`、`secrets` |
| `secrets[]` 元素 | `target`、`provider`、`secretPath`、`secretKey`、`configured`、`required` 等元数据；绝不返回 raw secret。 |
| 失败返回 | `xworkmate_secret_read_failed`、`xworkmate_context_failed`、`tenant_not_found`。 |

#### `PUT /api/auth/xworkmate/secrets/:target`

| 项 | 内容 |
| --- | --- |
| 路径参数 | `target`，例如 bridge auth token 对应的 secret target。 |
| 请求字段 | `value` |
| 成功返回 | `secret`、`profileScope`、`tokenConfigured` |
| 失败返回 | `xworkmate_secret_forbidden`、`read_only_account`、`xworkmate_secret_unknown_target`、`xworkmate_secret_value_required`、`xworkmate_secret_write_failed`、`xworkmate_profile_write_failed`。 |

#### `DELETE /api/auth/xworkmate/secrets/:target`

| 项 | 内容 |
| --- | --- |
| 路径参数 | `target` |
| 成功返回 | `secret`、`profileScope`、`tokenConfigured` |
| 语义 | 删除 Vault/backend secret，但保留 locator 元数据。 |
| 失败返回 | `xworkmate_secret_forbidden`、`read_only_account`、`xworkmate_secret_unknown_target`、`xworkmate_secret_delete_failed`。 |

## English

### Current Auth Model

`accounts.svc.plus` is not a pure JWT API. It is:

- session-first for the current control plane,
- primarily called through the `xc_session` cookie or `Authorization: Bearer <session-token>`,
- optionally wrapped by JWT middleware when `auth.enable` is on,
- protected by an internal service token for `/api/internal/*`,
- and protected by agent tokens for `/api/agent-server/v1/users|status`.

So “JWT enabled” does not mean “business logic runs only on JWT”; many handlers still resolve sessions explicitly.

### Authentication Modes

| Mode | Example APIs | Input location | Successful outcome |
| --- | --- | --- | --- |
| Session token | `/api/auth/login`, `/api/auth/session`, `/api/auth/xworkmate/*` | `Authorization` header or `xc_session` cookie | User context, admin permissions, XWorkmate profile access. |
| JWT refresh | `POST /api/auth/token/refresh` | JSON body `refresh_token` | A new `access_token`. |
| OAuth exchange code | `POST /api/auth/token/exchange` | JSON body `exchange_code` | The real session token, returned in both `token` and `access_token`. |
| Internal service token | `/api/internal/*` | `Authorization: Bearer <token>` | Trusted service-to-service reads. |
| Agent token | `/api/agent-server/v1/users`, `/api/agent-server/v1/status` | `Authorization: Bearer <token>` | Agent identity, client-list reads, status reporting. |

### Session Issuance Paths

The following flows mint a new session token:

| Entry point | Request fields | Success shape | Notes |
| --- | --- | --- | --- |
| `POST /api/auth/login` | `identifier/account/username/email` plus `password`, or email plus `totpCode` | `message`, `token`, `access_token`, `expiresAt`, `expires_in`, `user` | If MFA is enabled and no TOTP code is provided, the handler returns an MFA challenge instead of a session. |
| `POST /api/auth/register/verify` | `email`, `code` | `message`, `token`, `expiresAt`, `user` or `verified=true` | Existing-user email verification auto-logs the user in; pre-registration verification only marks the email as verified. |
| `POST /api/auth/password/reset/confirm` | `token`, `password` | `message`, `token`, `expiresAt`, `user` | A successful password reset immediately creates a session. |
| `POST /api/auth/mfa/verify` | `mfa_ticket/mfaToken`, `code/totpCode` | `message`, `token`, `access_token`, `expiresAt`, `expires_in`, `user` | Completes the MFA step after login. |
| `POST /api/auth/mfa/totp/verify` | `token`, `code` | `message`, `token`, `expiresAt`, `user` | First-time TOTP enablement also ends with a new session. |
| `GET /api/auth/oauth/callback/:provider` | query `code` | `307` redirect to frontend `/login?exchange_code=...` | The callback creates the session first, then issues a one-time exchange code instead of returning JSON directly. |

### Registration And Email Verification

#### `POST /api/auth/register/send`

| Item | Details |
| --- | --- |
| Request fields | `email` |
| Success | `200 {"message":"verification email sent"}` |
| Preconditions | Valid email format; not blacklisted; existing unverified users can still receive verification mail. |
| Failures | `invalid_request`, `invalid_email`, `email_blacklisted`, `smtp_timeout`, `verification_failed`, `email_already_exists`. |

#### `POST /api/auth/register`

| Item | Details |
| --- | --- |
| Request fields | `name`, `email`, `password`, `code` |
| Success | `201 {"message":"registration successful","user":...}` |
| Preconditions | Non-empty `name`; valid email; password length at least 8; when email verification is enabled, `code` must exist and match. |
| Side effects | Creates `store.User` and upserts a 7-day trial `store.Subscription`. |
| Failures | `name_required`, `missing_credentials`, `invalid_email`, `password_too_short`, `verification_required`, `invalid_code`, `email_already_exists`, `name_already_exists`. |

#### `POST /api/auth/register/verify`

| Item | Details |
| --- | --- |
| Request fields | `email`, `code` |
| Success A | `200 {"message":"email verified","token":"...","expiresAt":"...","user":...}` |
| Success B | `200 {"message":"verification successful","verified":true}` |
| Branching | Existing-user verification marks the user as verified and issues a session; pre-registration verification only confirms the code. |
| Failures | `token_in_query`, `invalid_request`, `invalid_code`, `verification_failed`, `session_creation_failed`. |

### Login, MFA Challenge, And Session Reads

#### `POST /api/auth/login`

| Item | Details |
| --- | --- |
| Request fields | `identifier`, `account`, `username`, `email`, `password`, `totpCode` |
| Standard success | `message`, `token`, `access_token`, `expiresAt`, `expires_in`, `mfaRequired=false`, `user` |
| MFA challenge | `message="mfa required"`, `mfaRequired=true`, `mfaMethod="totp"`, `mfaTicket`, `mfa_ticket`, compatibility field `mfaToken` |
| Behavior | Identifier resolution order is `identifier -> account -> username -> email`. If MFA is enabled and no TOTP code is provided, the handler returns a challenge instead of a session. |
| Failures | `credentials_in_query`, `invalid_request`, `missing_credentials`, `user_not_found`, `invalid_credentials`, `password_required`, `email_not_verified`, `sandbox_no_login`, `mfa_challenge_creation_failed`. |

#### `POST /api/auth/mfa/verify`

| Item | Details |
| --- | --- |
| Request fields | `mfa_ticket`, `mfaToken`, `code`, `totpCode`, `method` |
| Success | `message`, `token`, `access_token`, `expiresAt`, `expires_in`, `user` |
| Preconditions | Only `totp` is supported; the ticket must exist and not be expired; the user must have MFA enabled. |
| Failures | `mfa_ticket_required`, `mfa_code_required`, `unsupported_mfa_method`, `invalid_mfa_ticket`, `mfa_not_enabled`, `invalid_mfa_code`. |

#### `GET /api/auth/session`

| Item | Details |
| --- | --- |
| Auth | Requires a valid session. If token middleware is enabled, the request also passes JWT + active-user checks first. |
| Success | `{"user": ...}` and may include `tenantId`, `tenants`, and XWorkmate access metadata. |
| Failures | `session_token_required`, `invalid_session`, `session_user_lookup_failed`, `account_suspended`. |

#### `DELETE /api/auth/session`

| Item | Details |
| --- | --- |
| Auth | Requires a valid session. |
| Success | `204 No Content` |
| Side effect | Removes the process-local session and clears the cookie. |

### TOTP Provisioning, Status, And Disable

#### `POST /api/auth/mfa/totp/provision`

| Item | Details |
| --- | --- |
| Request fields | `token`, `issuer`, `account` |
| Success | `secret`, `otpauth_url`, `issuer`, `account`, `mfaToken`, `mfa`, `user` |
| Entry modes | Uses an existing MFA token or can bootstrap a new MFA challenge from the current session. |
| Failures | `invalid_request`, `invalid_mfa_token`, `mfa_token_required`, `invalid_session`, `mfa_already_enabled`, `read_only_account`, `mfa_secret_generation_failed`, `mfa_setup_failed`. |

#### `POST /api/auth/mfa/totp/verify`

| Item | Details |
| --- | --- |
| Request fields | `token`, `code` |
| Success | `{"message":"mfa_verified","token":"...","expiresAt":"...","user":...}` |
| Special failure | Repeated failures can lock the challenge and return `429 {"error":"mfa_challenge_locked","retryAt":"...","mfaToken":"..."}`. |
| Common failures | `mfa_token_required`, `invalid_mfa_token`, `mfa_secret_missing`, `mfa_code_required`, `invalid_mfa_code`, `mfa_update_failed`. |

#### `GET /api/auth/mfa/status`

| Item | Details |
| --- | --- |
| Inputs | query `token` / `identifier` / `email`, header `X-MFA-Token`, or `Authorization` session token. |
| Success | `enabled`, `mfa`, `user`. If queried by identifier and the user does not exist, it returns `200 {"mfa_enabled":false}`. |
| Failures | `mfa_status_failed`, `mfa_token_required`. |

#### `POST /api/auth/mfa/disable`

| Item | Details |
| --- | --- |
| Auth | Current session token, preferably from `Authorization`; also accepts query `token`. |
| Success | `{"message":"mfa_disabled","user":...}` |
| Failures | `session_token_required`, `invalid_session`, `mfa_disable_failed`, `mfa_not_enabled`, `read_only_account`. |

### OAuth And Exchange Code

#### `GET /api/auth/oauth/login/:provider`

| Item | Details |
| --- | --- |
| Path param | `provider`; the current code supports `github` and `google`. |
| Success | `307 Temporary Redirect` to the provider authorization URL. |
| Failure | `provider_not_found`. |

#### `GET /api/auth/oauth/callback/:provider`

| Item | Details |
| --- | --- |
| Input | query `code`; the `state` value may carry a frontend URL. |
| Success behavior | Exchanges the provider code, fetches the profile, creates or reuses the user, ensures the `store.Identity` binding exists, issues a session plus a one-time exchange code, then redirects to the frontend `/login?exchange_code=...`. |
| Failures | `provider_not_found`, `code_missing`, `oauth_exchange_failed`, `fetch_profile_failed`, `email_missing`, `email_not_verified`, `store_error`, `user_creation_failed`, `session_creation_failed`, `exchange_code_creation_failed`. |

#### `POST /api/auth/token/exchange`

| Item | Details |
| --- | --- |
| Request fields | `exchange_code` |
| Success | `token`, `access_token`, `token_type`, `expiresAt`, `expires_in`, `user` |
| Semantics | This does not mint a token from caller-provided claims. It converts the one-time code created during the OAuth callback back into the real session. |
| Failures | `invalid_request`, `invalid_exchange_code`, `session_user_lookup_failed`. |

### JWT Refresh

#### `POST /api/auth/token/refresh`

| Item | Details |
| --- | --- |
| Request fields | `refresh_token` |
| Success | `access_token`, `token_type="Bearer"`, `expires_in` |
| Preconditions | `h.tokenService != nil`; the refresh token must be valid and not expired. |
| Failures | `token_service_unavailable`, `invalid_request`, `invalid_refresh_token`. |

#### `POST /api/auth/refresh`

Alias route with the exact same behavior as `POST /api/auth/token/refresh`.

### Public Password Recovery

`POST /api/auth/password/forgot` and `/forgot/send-code` are public and enumeration-safe. The former emails a random token; the latter emails a six-digit code. `POST /forgot/confirm` and `/forgot/confirm-code` accept those credentials with the new password.

Challenges are persisted in `password_recovery_challenges`. The database stores only a SHA-256 digest of random tokens or a bcrypt hash of email codes, with expiry, failed-attempt, lockout, consumed, and invalidated state. Confirmation atomically consumes the challenge, updates only the password hash, and deletes that user’s existing session rows. Token confirmation then creates a new session. Challenges are one-time and expired credentials are rejected.

### Password Reset

#### `POST /api/auth/password/reset`

| Item | Details |
| --- | --- |
| Request fields | `email` |
| Success | `202 {"message":"if the account exists a reset email will be sent"}` |
| Implementation note | The handler itself is email-driven, but the route is mounted under the protected auth group, so enabling JWT middleware adds an extra precondition. |
| Failures | `email_in_query`, `invalid_request`, `email_required`, `password_reset_failed`, `read_only_account`. |

#### `POST /api/auth/password/reset/confirm`

| Item | Details |
| --- | --- |
| Request fields | `token`, `password` |
| Success | `message`, `token`, `expiresAt`, `user` |
| Preconditions | Password length at least 8; reset token must be valid; demo/read-only users are rejected. |
| Side effect | Revokes existing sessions before creating a new session; only the password hash changes. |
| Failures | `credentials_in_query`, `invalid_request`, `password_too_short`, `invalid_token`, `password_reset_failed`, `read_only_account`, `session_creation_failed`. |

### XWorkmate Profile And Vault-Backed Secrets

#### `GET /api/auth/xworkmate/profile`

| Item | Details |
| --- | --- |
| Success | `edition`, `tenant`, `membershipRole`, `profileScope`, `canEditIntegrations`, `canManageTenant`, `profile`, `tokenConfigured` |
| `profile` fields | `BRIDGE_SERVER_URL`, `bridgeServerOrigin`, `vaultUrl`, `vaultNamespace`, `vaultSecretPath`, `vaultSecretKey`, `secretLocators`, `apisixUrl` |
| Failures | `tenant_membership_required`, `tenant_not_found`, `xworkmate_context_failed`, `xworkmate_profile_read_failed`. |

#### `GET /api/auth/xworkmate/profile/sync`

| Item | Details |
| --- | --- |
| Success | `BRIDGE_SERVER_URL`, `BRIDGE_AUTH_TOKEN` |
| Semantics | A reduced sync view used by bridge / desktop flows instead of the full profile. |
| Failures | `tenant_membership_required`, `tenant_not_found`, `bridge_server_url_unavailable`, `bridge_auth_token_unavailable`. |

#### `PUT /api/auth/xworkmate/profile`

| Item | Details |
| --- | --- |
| Request fields | Accepts either a raw profile payload or `{ "profile": {...} }` |
| Forbidden fields | Raw token/password/api-key persistence is rejected with `token_persistence_forbidden`. |
| Success | Same shape as `GET /xworkmate/profile`. |
| Failures | `xworkmate_profile_forbidden`, `read_only_account`, `invalid_request`, `token_persistence_forbidden`, `xworkmate_profile_write_failed`. |

#### `GET /api/auth/xworkmate/secrets`

| Item | Details |
| --- | --- |
| Success | `tenant`, `membershipRole`, `profileScope`, `canEditIntegrations`, `vaultBackendEnabled`, `tokenConfigured`, `secrets` |
| `secrets[]` | Contains `target`, `provider`, `secretPath`, `secretKey`, `configured`, `required`, and similar metadata. It never returns the raw secret value. |
| Failures | `xworkmate_secret_read_failed`, `xworkmate_context_failed`, `tenant_not_found`. |

#### `PUT /api/auth/xworkmate/secrets/:target`

| Item | Details |
| --- | --- |
| Path param | `target`, for example the bridge-auth-token secret target. |
| Request fields | `value` |
| Success | `secret`, `profileScope`, `tokenConfigured` |
| Failures | `xworkmate_secret_forbidden`, `read_only_account`, `xworkmate_secret_unknown_target`, `xworkmate_secret_value_required`, `xworkmate_secret_write_failed`, `xworkmate_profile_write_failed`. |

#### `DELETE /api/auth/xworkmate/secrets/:target`

| Item | Details |
| --- | --- |
| Path param | `target` |
| Success | `secret`, `profileScope`, `tokenConfigured` |
| Semantics | Deletes the Vault/backend secret while preserving locator metadata. |
| Failures | `xworkmate_secret_forbidden`, `read_only_account`, `xworkmate_secret_unknown_target`, `xworkmate_secret_delete_failed`. |
