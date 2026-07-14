# docs/tasks — 任务归档(项目记忆)

每次任务对话结束后,在此按**任务**建一个 Markdown 文件,记录:目标、结论、关联 PR(带 ID + 状态)、踩坑、遗留待办。作为跨会话的全局项目记忆,便于回溯"某个功能是怎么上线的、改了哪些 PR、还有什么没做"。

## 约定

- 文件名:`YYYY-MM-DD-<kebab-任务名>.md`
- 每个文件顶部有状态块(Status / Date / Related PRs)
- PR 用完整链接,标注 [MERGED]/[OPEN]/[CLOSED]
- 跨仓联动的任务,在每个相关仓库的 `docs/tasks/` 各留一份(以本仓视角),互相引用

## 索引

| 日期 | 任务 | 状态 | 关联 PR |
|---|---|---|---|
| 2026-07-14 | [用户自助:找回密码 · MFA 恢复码 · 自助注销](2026-07-14-self-service-recovery-and-deletion.md) | 🟢 找回密码已合并;恢复码/注销待实现(决策已锁) | accounts [#26](https://github.com/ai-workspace-services/accounts/pull/26) [MERGED] |
| 2026-07-14 | [高级服务就绪度门禁(邮箱+密码+MFA)+ 渐进引导](2026-07-14-advanced-service-readiness-gate.md) | ✅ 已合并(经本次恢复 PR;原 #25 误合进已脱钩分支,代码从未进入 main,见文件内说明) | accounts #25(名义)→ 实际经恢复 PR 落地;console 配套待做 |
| 2026-07-14 | [OAuth 登录后邮箱验证门禁 + trial 激活](2026-07-14-oauth-email-verify-gate.md) | ✅ 已合并;console 配套待做 | accounts [#24](https://github.com/ai-workspace-services/accounts/pull/24) [MERGED] |
| 2026-07-14 | [CI 环境路由:main→uat,release/tag→prod](2026-07-14-ci-uat-prod-env-routing.md) | ✅ 已合并;需设 UAT_TARGET_HOST 才能实际部署 uat | accounts [#27](https://github.com/ai-workspace-services/accounts/pull/27) [MERGED] |
| 2026-07-13 | [Stripe 计费打通 —— 进度与现状(交接快照,已过期)](2026-07-13-stripe-billing-status.md) | 🧭 见最新汇总文档 | accounts [#23](https://github.com/ai-workspace-services/accounts/pull/23) [MERGED] |
| 2026-07-12 | [Stripe 计费 P1.5(欠费 suspend 断流 + 清欠恢复)](2026-07-12-stripe-billing-p15.md) | ✅ 已合并 | accounts [#30](https://github.com/ai-workspace-services/accounts/pull/30) [MERGED];billing-service [#11](https://github.com/ai-workspace-services/billing-service/pull/11) |
| 2026-07-11 | [Stripe 计费 P1(目录/审计/entitlement sync)](2026-07-11-stripe-billing-p1.md) | ✅ 已合并部署 | accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19) [MERGED];前置 #18 + playbooks #121 |
| 2026-07-11 | [OAuth 注册登录(GitHub/Google)](2026-07-11-oauth-login.md) | ✅ GitHub+Google 均上线 | accounts #12 #13 #14 #16;playbooks #111 #112 |
