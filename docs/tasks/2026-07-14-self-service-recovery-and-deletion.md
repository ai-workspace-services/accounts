# 用户自助:找回密码 · MFA 恢复码 · 自助注销

> **Status**: 🟢 找回密码已实现(本 PR)· ⬜ MFA 恢复码 + 自助注销待实现(决策已锁,见下)
> **Date**: 2026-07-14
> **Related PRs**: 本 PR(accounts,分支 `feat/self-service-recovery` —— 仅含公开找回密码)
> **关联**: [OAuth 邮箱验证门禁](2026-07-14-oauth-email-verify-gate.md) · [高级服务就绪度门禁](2026-07-14-advanced-service-readiness-gate.md)

## 需求(三项自助能力)

1. 邮箱重设密码 / 找回密码
2. MFA 恢复(设备丢失死锁)+ 忘密找回
3. 自助注销账户(有 MFA 要先解绑)、退还未消费的按量付费余额

## 已拍板决策(2026-07-14)

| # | 决策点 | 结论 |
|---|---|---|
| 1 | MFA 设备丢失恢复机制 | **启用 MFA 时生成一组一次性恢复码**(行业标准,不削弱 MFA;邮箱被盗也无法绕过 MFA) |
| 2 | 自助注销模式 | **软删除 + 30 天冷静期**(标记 `pending_deletion`,订阅期末终止,窗口内可撤销,到期清除)——与 Stripe 规划 §4 P2「注销联动:30 天删除冷静期」一致 |
| 3 | 退还未消费余额 | **注销时记录待退金额 + 原支付方,实际 Stripe 退款由 billing-service / 运营执行**;accounts 不直接调 Stripe refund(解耦资金动作,契合 P2 尚未落地的现状) |

> 关键安全澄清:找回密码本身不绕过 MFA —— 邮件重置密码后,登录仍走 MFA 挑战。恢复码解决的是「MFA 设备也没了」的死锁。

## 本 PR 已实现:公开找回密码

**根因**:`/api/auth/password/reset[/confirm]` 原挂在 `authProtected`(需 session)下 —— 忘了密码登不进来的用户根本够不到。而两个 handler 其实:`requestPasswordReset` 防枚举(统一 202)、无 session 依赖;`confirmPasswordReset` 纯 reset-token —— 本就适合公开。

**改动**(`api/api.go` 路由):新增公开别名,复用现有 handler
- `POST /api/auth/password/forgot`(= requestPasswordReset,公开)
- `POST /api/auth/password/forgot/confirm`(= confirmPasswordReset,公开)
- 原 `authProtected` 下的 `/password/reset[/confirm]` 保留,作为已登录「修改密码」场景。

**测试**:`TestPublicForgotPasswordFlow`(forgot→邮件 token→confirm→新密码生效→token 单次性)、`TestForgotPasswordUnknownEmailIsEnumerationSafe`(未知邮箱仍 202、不发信)。`go test ./... && go vet` 全过。

## 待实现 A:MFA 恢复码(下一增量)

- **持久化**:`store.User` 加 `mfa_recovery_codes`(存 **hash 后**的码;复用 postgres store 的 `encodeStringSlice`/`decodeStringSlice` + `caps.hasXxx` 列探测模式,同 `groups`);schema.sql `users` 加列;memory store 同步。
- **生成**:`verifyTOTP`(MFA 确认启用)成功时生成一组(如 10 个)一次性码,**明文仅本次返回一次**,库里存 hash。
- **消费**:`verifyMFALogin` 支持用恢复码替代 TOTP;命中即作废该码(从集合移除)。
- **端点**:`POST /api/auth/mfa/recovery-codes/regenerate`(authed,重新生成并作废旧的);`GET /api/auth/mfa/recovery-codes/status`(剩余数量,不回显明文)。
- **解绑**:`disableMFA` 支持凭恢复码执行(设备丢失时解绑)。

## 待实现 B:自助注销(软删 + 冷静期 + 退款记录)

- **schema/User**:加 `deletion_state`(active|pending_deletion)+ `deletion_scheduled_at TIMESTAMPTZ`(= now+30d)。
- **发起** `DELETE /api/auth/account`(authed):若 `MFAEnabled`,先校验一次 MFA(TOTP 或恢复码)作为不可逆操作的身份证明 → 置 `pending_deletion` + `deletion_scheduled_at` → 解绑 MFA → 订阅 `cancel_at_period_end`(复用 `cancelSubscription`)→ 发 `billing_events`(type `account_deletion_requested`,带 `CurrentBalance` 待退 + 原支付方,供 billing-service/运营退款)。
- **撤销** `POST /api/auth/account/restore`(authed,窗口内):清 `deletion_state`/`deletion_scheduled_at`,恢复接入。
- **清除 job**:后台定时扫 `deletion_state='pending_deletion' AND deletion_scheduled_at < now()` → 硬删除(复用 P1.5 arrears sweep 的 ticker 模式)。
- **门禁联动**:`pending_deletion` 期间,`listAgentUsers`/`internalNetworkIdentities` 视同停用(不下发代理),与 §P1.5 suspend 过滤并列。
- **退款执行**:accounts 只记录;billing-service/运营读 `billing_events` 或待退表,调 Stripe refund 原路退 `CurrentBalance`(P2 Stripe 集成就绪后打通)。

## 遗留待办

- [ ] console 前端:找回密码入口(登录页「忘记密码?」→ `/password/forgot`);MFA 恢复码展示/下载/再生成;注销确认(二次确认 + MFA 校验 + 冷静期提示 + 待退金额展示)。
- [ ] 实现待实现 A(MFA 恢复码)、B(自助注销)—— 决策已锁,可直接开工。
- [ ] 与 billing-service 对齐退款消费端(`billing_events` 契约或待退表)。
