# TEAM-1：个人与企业团队的专属节点授权

项目：https://github.com/orgs/ai-workspace-xstream/projects/1

本批次只允许本地自测和 UAT 测试。不得访问或变更生产 Web SaaS、PROD 主机；本轮 PR 保持候选状态，不发布生产标签或触发生产部署。

## 最小步骤

1. Accounts（TEAM-1）：增量团队记录、管理员开通、访问范围与 Agent 凭据过滤。
2. XConnect App（TEAM-2）：同一 App 的团队模式、登录同步、撤销和缓存隔离。
3. Portal + Knowledge（TEAM-3）：团队定制展示、人工管理、授权订阅导出。
4. Edge Agent（TEAM-4）：验证空用户列表、成员移除和存量连接撤销；复用现有重启受影响 Xray 实例的路径。
5. 联合 UAT（TEAM-5）：个人、团队 A/B、过期/移除成员逐项验证并保存版本、回退点和实际连接证据。

## 数据与兼容

`2026101201_network_teams.up.sql` 只新增 `network_teams`、`network_team_members`；不改写现有用户、凭据、套餐、财务与用量历史。没有成员记录的账号默认个人模式。成员表以用户 UUID 为主键，保证一个用户最多一个团队；移除后保留 `dedicated_only=true`，只有显式设置 `mode=personal` 才恢复公共访问。

成员上限检查、成员变更和审计在同一事务内，数据库锁用于跨实例并发控制。团队有效期独立于个人订阅；有效团队成员不会因沿用 Free 标签且长时间少用被自动归档。既有停用与欠费访问暂停继续生效。

先在一次性数据库执行 `sql/migrations/tests/network_teams_upgrade_test.sql`，验证重复应用与旧用户哨兵不变，再审核 UAT 增量迁移。先迁移数据库，再部署候选代码。缺失团队表时接口拒绝下发，不能回退全量授权。

## API

| 操作 | 路径 |
|---|---|
| 当前账号访问范围 | GET `/api/auth/network-access` |
| 管理员团队列表 | GET `/api/auth/admin/network-teams` |
| 开通/更新团队 | PUT `/api/auth/admin/network-teams/:teamId` |
| 查看/修改成员授权 | GET/PUT `/api/auth/admin/users/:userId/network-access` |

团队请求字段：`name`, `memberLimit`, `validFrom`, `validUntil`, `active`, `reason`；时间采用带时区 RFC3339。成员请求字段：`mode`（personal/dedicated）, `teamId`, `reason`。`mode=dedicated, teamId=""` 表示撤销并保持受限。

专属 Agent 使用独立凭据，配置分组 `team:<teamId>`（可同时带 `access:dedicated-only`）。旧公共凭据不含团队分组，保持个人用户下发行为。专属凭据不能更换 Agent ID；共享公共凭据不能覆盖已绑定团队的 Agent。节点列表、区域发现、完整配置、Agent 用户列表与事件流按范围过滤；团队故障返回不可用，完整配置只包含授权专属入口。

Agent 凭据的分组由 Accounts 配置维护，不能从节点自报的 pool、域名或请求参数推导团队权限。`team-admin:<teamId>` 是用户管理角色，不能用作节点分组。用户列表、事件流、状态上报对未绑定团队的 `access:dedicated-only`、多团队绑定、用户管理员标记和大小写错误的团队标记统一返回 `403 agent_scope_invalid`，即使节点使用凭据自己的 Agent ID 也不例外。没有这些保留标记的旧公共凭据仍只下发个人用户。到期或停用团队的客户端列表为空；已移除成员停止下发，不能自动恢复公共授权。

## UAT 迁移顺序

1. 新团队先保存为 `active=false`，配置专属节点与独立凭据。
2. 将成员置于专属受限模式，撤销旧公共/旧团队授权。
3. 等待相关公共/旧团队 Agent 报告新应用版本，并实际确认旧链接和已有连接已失效。
4. 绑定到目标团队、启用团队，验证专属连接。直接从个人/其他团队加入已启用团队会返回 `network_withdrawal_required`；数据库也执行此检查。
5. 已启用团队增员同样先撤销旧范围，再验证并绑定。回退保持受限，不能恢复旧版全量用户下发。

## 自测与未完成门槛

`go test ./api ./internal/store ./internal/agentserver`；并发成员上限、跨团队/公共拒绝、到期、节点故障、共享凭据别名、事务审计回滚均有测试。实际 PostgreSQL 并发测试只在明确指定的新建一次性数据库执行 `NETWORK_TEAM_TEST_DSN=... go test ./internal/store -run NetworkTeamPostgresConcurrent`，默认跳过。

代码和自测通过不代表 UAT 验收。Project 卡保持开放，完成节点真实撤销、App 真机和 UAT 增量迁移验证后再更新验收状态。

## 2026-10-12 本机 SIT 记录

候选 PR：Accounts #211、XConnect App #106、Portal #414、Knowledge #146；均未合并，未发布标签或部署生产。

本机 Accounts 绑定 `127.0.0.1:18081`，Portal 绑定 `127.0.0.1:13000`。数据库使用本机 OrbStack 的独立 `codex-team-sit-20261012` PostgreSQL 17 容器，端口只绑定 `127.0.0.1:55439`，业务数据库 `sit_accounts` 只含测试数据。启动进程使用清理后的环境变量，未继承共享数据库、SMTP、OAuth 或 Agent 同步配置；Portal 选择全部指向 loopback 的 dev runtime 配置。运行凭据、PID 和日志仅保存在本机权限 0700 的 `/tmp/codex-team-sit`，凭据不纳入仓库和 Project。

已通过：

- Accounts API/store/agentserver、SQL/native/full-business、配置和运行角色相关单测。
- PostgreSQL 17 原生空库初始化、前向升级、旧版本全业务复制和精度/审计数据一致性测试。
- 团队增量迁移重复执行、RLS 检查、旧用户哨兵保留、真实并发成员上限及审计回滚。
- 18 项 localhost HTTP 联调（11 项管理流程 + 7 项成员授权）：匿名管理拒绝、团队创建、Portal 授权转发、成员创建、成员上限、团队启用、移除保留受限、活跃团队迁入撤销门槛、显式恢复个人；新个人账号兼容、成员专属策略、空专属节点及完整配置 503、成员撤销后节点及配置 403。

原生快照及全业务契约同步新增两表：原生业务表 54 张、含 Billing 的全业务表 55 张，当前迁移版本 `2026101201`。旧源库可以缺失团队两表，目标保持空表，已有用户继续个人模式。

仍未完成：GitHub 托管 managed-runtime PostgreSQL 专项检查、App macOS 签名/真实隧道、专属 Edge 实例存量连接撤销、UAT 增量迁移。本机 managed-runtime 专项 fixture 受已有 GitHub-hosted/5432 防误用门槛限制，不冒充 CI 条件；由 PR CI 验证。App GitHub macOS 打包另遇现有浮动 `connectivity_plus` 与 runner Xcode SDK 不兼容，Flutter 团队逻辑验证已通过，打包错误不能当成业务测试通过。
