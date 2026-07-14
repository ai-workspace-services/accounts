# 高级服务就绪度门禁(邮箱 + 密码 + MFA)+ 渐进引导

> **Status**: ⏳ 代码完成 + 测试通过,待合并部署(**栈式依赖 #24**,需 console 前端配套)
> **Date**: 2026-07-14
> **Related PRs**: 本 PR(accounts,分支 `feat/advanced-service-readiness`,base = `feat/oauth-email-verify-gate` / [#24](https://github.com/ai-workspace-services/accounts/pull/24))
> **关联**: [OAuth 邮箱验证门禁](2026-07-14-oauth-email-verify-gate.md)(下层:锁代理接入)

## 目标

高级服务(专属节点托管、对接外部云的专属托管资源等)要求账号完成三件安全加固:**验证邮箱 + 设置密码 + 启用 MFA**。三者齐全前,console 只展示引导/介绍页,**激活被拒**。用**渐进引导**(逐项打勾)推动用户升级。

这是比 [#24](https://github.com/ai-workspace-services/accounts/pull/24) 更高一层的门禁:

| 层级 | 门禁 | 解锁 |
|---|---|---|
| 基础(代理/VPN) | 邮箱验证(#24) | xray client 下发 + TRIAL-7D |
| **高级服务(本 PR)** | 邮箱验证 **+ 密码 + MFA** | 专属托管类服务激活 |

## 现状前提

- 高级服务端点**尚不存在**(当前仅代理相关)。故本 PR 交付**就绪度判定 + 可复用门禁 + 引导 API + OAuth 设密码**这套 scaffold,未来的高级服务 handler 首行调 `requireAdvancedServiceReadiness` 即接入。
- 三个信号基建齐全:`EmailVerified`(generated 列)、`PasswordHash`、`MFAEnabled`;MFA 流程(`/mfa/totp/provision|verify|disable`)与密码 reset 流程均在。

## 实现(新增 `api/service_readiness.go`)

- **`computeServiceReadiness(user)`**:按渐进顺序(email → password → mfa)算出每项 `met` + 整体 `ready` + `nextStep`(首个未达成项的 key)。纯函数,易测。
- **`GET /api/account/service-readiness`**:返回上述结构,驱动 console 渐进引导 UI。
- **`requireAdvancedServiceReadiness(c)`**:可复用门禁。未就绪 → `403 advanced_service_locked` + `intro:true` + 完整 readiness 状态(console 据此显示引导页而非服务);就绪 → 返回 user 放行。高级服务 handler 首行调用即可。
- **`POST /api/auth/password/set`**:已认证 + 已验证邮箱、**尚无密码**的用户(典型即 OAuth 用户)直接设密码,免邮件往返(session 已证明账号控制权)。已有密码 → `409`,引导走 reset 轮换;未验证邮箱 → `403`(密码是验证之后的引导步)。
- **`/auth/me` 增补**:`passwordSet`(bool)+ `serviceReadiness`(完整结构),前端一次拉齐引导所需状态。

## 渐进引导用户旅程

```
OAuth 登录(邮箱已验证、代理已解锁,见 #24)
   │  想用「专属节点托管」→ 点激活 → 后端 requireAdvancedServiceReadiness
   │      未就绪 → 403 intro=true + readiness{nextStep: "password"}
   ▼  console 展示引导页,高亮下一步
POST /api/auth/password/set            → passwordSet=true, nextStep 前进到 "mfa"
POST /api/auth/mfa/totp/provision+verify → mfaEnabled=true, ready=true
   ▼  再次激活 → 门禁放行 → 服务可用
```

## 测试

`go test ./... && go vet ./...` 全过。
- `TestComputeServiceReadiness`:四种状态(全缺 / 仅验证 / 验证+密码 / 三全)→ ready + nextStep 正确。
- `TestServiceReadinessEndpoint`:未就绪用户端点返回 nextStep=email。
- `TestSetPasswordFlow`:短密码 400 → 有效 200 落库 → 重复设 409。
- `TestSetPasswordRequiresVerifiedEmail`:未验证邮箱设密码 403。
- `TestRequireAdvancedServiceReadinessGate`:未就绪 403+intro,就绪放行。

## 遗留待办

- [ ] **console 前端配套**(console.svc.plus 仓):读 `/auth/me` 的 `serviceReadiness` 或 `GET /api/account/service-readiness`,渲染渐进引导卡片(邮箱✓/密码/MFA);高级服务入口未就绪时置引导态;接 `/password/set` 与现有 MFA 端点。
- [ ] **接第一个真实高级服务时**:handler 首行 `user, ok := h.requireAdvancedServiceReadiness(c); if !ok { return }`,并把 403 的 `intro`/`readiness` 交给前端渲染介绍页。
- [ ] 合并顺序:先 [#24](https://github.com/ai-workspace-services/accounts/pull/24)(base)后本 PR;#24 合并后本分支 rebase 到 main。
- [ ] 可选:MFA/密码等安全加固项做成配置化"高级服务要求清单",便于未来按服务分级(当前三项硬编码)。
