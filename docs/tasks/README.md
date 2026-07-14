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
| 2026-07-11 | [OAuth 注册登录(GitHub/Google)](2026-07-11-oauth-login.md) | ✅ GitHub+Google 均上线 | accounts #12 #13 #14 #16;playbooks #111 #112 |
