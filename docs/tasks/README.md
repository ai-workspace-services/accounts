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
| 2026-07-14 | [OAuth 登录后邮箱验证门禁 + trial 激活](2026-07-14-oauth-email-verify-gate.md) | ⏳ 代码完成待合并(需 console 配套) | accounts 本 PR |
| 2026-07-13 | [**Stripe 计费打通 —— 进度与现状(交接快照)**](2026-07-13-stripe-billing-status.md) | 🧭 P0✅上线 P1✅合并 P1.5🟡待合并 P2/P3⬜ | 见文内总表 |
| 2026-07-11 | [Stripe 计费 P1(目录/审计/entitlement sync)](2026-07-11-stripe-billing-p1.md) | ✅ 已合并部署 | accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19);前置 #18 + playbooks #121 |
| 2026-07-11 | [OAuth 注册登录(GitHub/Google)](2026-07-11-oauth-login.md) | ✅ GitHub+Google 均上线 | accounts #12 #13 #14 #16;playbooks #111 #112 |
