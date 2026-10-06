# Accounts 在数据库切换期间的运行角色

这是服务运行合同；切换授权仍由 Toolkit 的真实数据证据和 Edge 门禁决定。

| 变量 | 合同 |
| --- | --- |
| `DATABASE_RUNTIME_ROLE` | `primary` 或 `standby`；未配置为现有 LEGACY 启动路径 |
| `DATABASE_BACKGROUND_WRITERS` | 管理角色必须显式 `true` / `false`；standby 只允许 false |
| `DATABASE_URL` | 唯一运行时数据库连接；不接受残留 `SUPABASE_CONNECT_URI` / `SUPABASE_CONNECT_URL` |
| `DATABASE_IDENTITY_SHA256` | 经审核 Host/Port/Database/Role JSON 的 SHA-256，不含密码；匹配配置路由，不证明实际物理库或数据一致 |
| `IMAGE` | 预构建 full-SHA `service_image_ref`，探针 tag/commit/version 均从它解析 |

管理主库必须为原生 Selfhost PostgreSQL 17 `account` 库、干净版本 `2026100701`、完整编译的 53 表/列/PK/FK。
启动仅只读核验目录，不读全量业务行、不改 schema；不运行 AutoMigrate、历史 SQL、默认用户/租户/资料/Billing catalog/queue 初始化，
也不启动 Sandbox Proxy UUID 自动轮换。所有数据准备须由经审核的数据库 owner 完成。
Accounts API 管理角色不启动配置里的 bundled Agent；独立 Agent 部署保留自己的生命周期。
`primary + background=false` 仍提供正常业务 API，后台额度/订阅/agent cleanup/Xray 同步保持暂停；
只有来源冻结、完整最终追平/一致性与单写者门槛通过后才允许启动这个角色并将生产入口切来。
后台 true 是后续受控激活，不能在旧主库仍写入时启用。

待机不构建任何业务 store/ORM/后台任务，因此也可用于冻结旧 Serverless 应用，而无需对旧 schema 做迁移。
它只提供 GET/HEAD `/api/ping`、`/healthz`、`/readyz`；业务请求（包括 GET/OPTIONS）一律 503，readyz 503。
Cloud Run 和 Selfhost 都经管理的 HTTPS ingress/Caddy 终止 TLS，应用待机端口使用明文私网/容器入口。
健康探针不是业务可用性，不证明实际数据库连接成功；待机 schema_version 为 0。
它不关闭其他 Cloud Run revision、Supabase 任务或任何直接数据库写者，来源冻结仍须外部 owner 核验全范围。

主库探针附加 `database_role`、`configured_database_sha256`、`schema_version`、`schema_management=external`、
`bootstrap_writes=false`、`proxy_uuid_rotator=false`、`background_writers`、`business_requests_enabled`。
主库 readyz 仍执行实际数据库健康检查，不能被角色元数据替换成固定成功。`bootstrap_writes` 仅指直接 schema/seed 初始化，
后台 true 时正常业务后台写入仍可能发生。所有探针仍使用同一个 IMAGE 解析器，不接收独立 COMMIT_ID。

资格范围：新的 PostgreSQL 17 隔离测试以 SELECT-only 角色启动原生数据集，比对所有业务行及 schema 摘要；
待机测试使用不存在的 DB，验证不会连接或启动 Agent。CI 和这些回执不代替生产源冻结、53 表最终一致性、单写者或实际入口验收。
生产当前仍保持 Serverless 主库。

LEGACY 启动仅供尚未转换的部署保留。受控 owner/caller 与真实入口资格完成后再审查其删除；
管理角色拒绝不匹配的 schema，不执行过渡兼容或向下回放。
