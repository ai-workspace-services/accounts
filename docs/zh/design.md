# 设计

本页是 `accounts.svc.plus` 当前设计取舍的中文入口。

## 主设计记录

当前最重要的设计记录是 [architecture/design-decisions.md](../architecture/design-decisions.md)。其中固化了当前代码库真实成立的取舍，包括：

- 控制面采用 session-first，而不是 JWT-only。
- 主业务持久化通过 `store.Store` 抽象边界统一暴露。
- GORM 仅用于部分管理面模型，而不是替代整个 store。
- agent 侧通过 `agentserver.Registry` 做预共享 token 认证与状态聚合。
- XWorkmate 采用 secret locator + Vault-backed secret 持久化方案。
- Xray 配置采用 generator + periodic sync 的生成式控制模型。

## 建议阅读顺序

1. [设计决策](../architecture/design-decisions.md)
2. [架构总览](../architecture/overview.md)
3. [认证与鉴权](../api/auth.md)

## 当前设计摘要

- 运行时真相优先来自 store 与运行时契约，而不是本地 config-center 的重复状态。
- session 与 OAuth exchange code 由数据库持久化；密码找回挑战由 `password_recovery_challenges` 持久化。MFA challenge、注册邮箱验证码仍为进程内状态。
- 管理员权限矩阵和首页视频配置被有意拆到 GORM-backed service，而不是塞进主业务 store。
- agent mode 与 server mode 复用同一套 Xray 生成能力，而不是引入第二套配置模型。

## 相关页面

- [架构](architecture.md)
- [开发手册](developer-guide.md)
