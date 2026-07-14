# OAuth 登录后邮箱验证门禁 + trial 激活

> **Status**: ⏳ 代码完成 + 测试通过,待合并部署(**需配套 console 前端改动**,见 §遗留)
> **Date**: 2026-07-14
> **Related PRs**: 本 PR(accounts,分支 `feat/oauth-email-verify-gate`)
> **背景关联**: [Stripe 计费打通交接快照](2026-07-13-stripe-billing-status.md) · [P1 目录/entitlement](2026-07-11-stripe-billing-p1.md)

## 目标

GitHub/Google OAuth 用户可以**默认登录**(进 console),但**代理/VPN 接入**要等他们完成一次**我方邮箱验证**(收码→输码)才解锁;验证通过的同一步**自动发放 7 天 TRIAL-7D**(订阅 + 套餐档案 + 配额)。

## 决策(2026-07-14 拍板)

| # | 问题 | 结论 |
|---|---|---|
| 1 | "验证绑定邮箱"语义 | **真发验证码到 OAuth 邮箱**,用户输码核验(证明收件箱可达 → P1.5 催缴邮件有落点 + 防一次性号刷试用) |
| 2 | "全部功能"门禁范围 | **代理/VPN 接入**(xray client 下发);console 仍可登录,好让用户走验证流程 |
| 3 | trial 激活时机 | **验证通过即自动发放**(一步到位,无需再点"激活") |

## 现状前提(改动前)

- OAuth 首登(`oauthCallback`)直接:`Active=true` + `EmailVerified=true`(信任 provider)+ 立即发 TRIAL-7D + 套餐档案。→ 一登录即满配,**没有任何验证/激活关卡**。
- "全部功能"实际只认 `user.Active`:`RequireActiveUser` 只查 Active;`listAgentUsers` / `internalNetworkIdentities` 只给 `Active` 用户下发 xray 配置 / 归属。`EmailVerified` 此前不参与门禁。
- 邮箱验证基建已具备:`POST /api/auth/register/send`(发 6 位码)+ `/register/verify`(核验置 verified)+ `internal/mailer`。
- `email_verified` 是 **generated 列**(`email_verified_at IS NOT NULL` 派生)→ 存量已验证用户不受影响,只有新 OAuth 用户为 false。

## 实现

### 门禁(代理层加 EmailVerified)
- `api/agent_server.go` `listAgentUsers`:sandbox 特例之后加 `if !u.EmailVerified { continue }` —— 未验证用户拿不到 xray client。sandbox 豁免。
- `api/internal_network_identities.go`:同样加 `EmailVerified` 过滤(sandbox 豁免),与代理下发口径一致,避免未验证用户流量被错误归属。
- `RequireActiveUser` **不动**(console 保持可达,用户才能进去验证)。

### OAuth 回调(`api/api.go` `oauthCallback`)
- **新用户**:`EmailVerified=false`(provider 验证是必要非充分条件)+ **不发 trial / 不发档案**。仍建 session + 跳转 console(未验证也能登录)。
- **返回用户**:**移除**原"未验证则自动置 EmailVerified=true"逻辑 —— 否则未验证 OAuth 用户反复登录即绕过门禁。已验证用户不降级。

### trial 发放(收敛为单一入口)
- 新增 `provisionOnboardingTrial(ctx, userID)`:发 TRIAL-7D 订阅 + 调 `provisionTrialEntitlements`(套餐档案 + 配额)。**首次邮箱验证**时触发。
- `verifyEmail` 已存在用户分支:`if !user.EmailVerified` 内置 verified 后调用它 → OAuth 用户输码即激活 trial + 解锁代理(下次 agent sync 生效)。
- 密码注册流程(`register`)重构为复用同一 helper(行为不变)。

## 用户旅程(改动后)

```
OAuth 登录 → 账号建成(Active,未验证,无 trial)→ 进 console
   │  console 检测 /auth/me 的 emailVerified=false → 提示"验证邮箱解锁"
   ▼
POST /register/send{email} → 收码
POST /register/verify{email,code} → EmailVerified=true + 自动发 TRIAL-7D(档案+配额)
   ▼
下次 agent sync → 节点下发 xray client → 代理可用
```

## 测试

`go test ./... && go vet ./...` 全过。
- `TestOAuthDefersTrialUntilEmailVerified`(重写):OAuth 后无 trial/档案 → send+verify → 档案+配额到位。
- `TestAgentServerUsers_ExcludesUnverifiedUsers`(新增):Active 但未验证用户不入 proxy client;已验证用户在。
- `TestAgentServerUsers_DefaultSyncIncludesSandboxAndRegularUsers`(修正):normal 用户改为已验证(原测本意=验证 UUID 过期不阻断同步)。

## 遗留待办

- [ ] **console 前端配套**(console.svc.plus 仓,本 PR 不含):`emailVerified=false` 时展示验证引导页 + 接 `/register/send` 与 `/register/verify`;未验证时代理相关入口置灰并提示。
- [ ] 合并部署后回归:新 GitHub/Google 账号 → 未验证 → 无代理 → 收码验证 → 代理解锁 + trial 生效。
- [ ] 存量未验证但 Active 的用户(若有)会被新门禁挡在代理外 —— 上线前用 SQL 抽查 `users` 中 `active AND email_verified_at IS NULL` 的数量确认影响面(预期为 0,因历史各建号路径均置 verified)。
- [ ] 与 P1.5(欠费→suspend 断流)叠加验证:两道门禁(未验证 / 已 suspend)互不干扰。
