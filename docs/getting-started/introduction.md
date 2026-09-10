# 项目介绍

XWorkmate Account Service（账号服务）负责用户注册、登录、会话管理、MFA、订阅状态与管理员权限矩阵等能力，同时提供与 Xray 节点协同的代理模式与跨区域数据同步工具。

## 解决的问题

- 统一账号体系：注册/登录/会话/密码重置
- 安全增强：邮件验证、TOTP 多因素认证
- 运营支持：订阅状态管理、用户指标统计
- 运维协同：Agent 模式同步 Xray 配置与状态
- 跨区数据：导入/导出与同步工具（`migratectl` / `syncctl`）

## 关键特性（基于当前代码）

- HTTP API：基于 Gin，默认端口 `:8080`，健康检查 `GET /healthz`
- 用户体系：用户名、邮箱、角色/等级、权限组
- 邮件能力：SMTP 可选；未配置 SMTP 时自动关闭邮件验证
- 会话管理：服务进程内存会话（非持久化）
- JWT 令牌服务：可选启用（public/access/refresh）
- 管理能力：管理员权限矩阵、用户指标与 Agent 状态接口
- Xray 侧同步：定期生成 Xray 配置并可触发校验/重启

## 运行模式

- `server`：仅启动账号 API 服务
- `agent`：仅启动 Agent，同步 Xray 配置并上报状态
- `server-agent` / `all` / `combined`：服务 + Agent 同时运行

## 代码位置速览

- 入口：`cmd/accountsvc/main.go`
- API：`api/`
- 配置：`config/`
- 数据库：`sql/`
- 工具：`cmd/migratectl`、`cmd/syncctl`、`cmd/createadmin`
- 运行脚本：`scripts/`

下一步：阅读 `getting-started/quickstart.md` 完成最小启动示例。
