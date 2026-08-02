# Account 数据库结构与双向同步指南

## RBAC / Root 迁移

- 新增迁移脚本：`sql/20260204_rbac_root_constraints.sql`
- 目的：
  - 创建 RBAC 元数据表（`rbac_roles` / `rbac_permissions` / `rbac_role_permissions`）
  - 增加 root 唯一约束（仅允许一个 `role=root`）
  - 增加 root 邮箱约束（`role=root` 必须是 `admin@svc.plus`）

执行示例：

```bash
psql "$DB_URL" -v ON_ERROR_STOP=1 -f sql/20260204_rbac_root_constraints.sql
```

## Portal / Xray canonical UUID

`users.uuid` is the canonical account identity. The legacy `proxy_uuid` column
is retained for compatibility but must always equal `users.uuid`; Xray client
configuration and Portal QR generation use the canonical account UUID.

For a controlled UAT rollout, apply the idempotent backfill once:

```bash
psql "$DB_URL" -v ON_ERROR_STOP=1 -f sql/20260803_proxy_uuid_equals_user_uuid.sql
```

The migration updates existing rows, removes the independent database default,
and installs `users_proxy_uuid_matches_uuid_ck`. Production must not be changed
as part of the UAT validation.

## 方案概览

| 目标 | 推荐方案 | 说明 |
| --- | --- | --- |
| ✅ 一地写入 → 异步同步另一地 | [pgsync](#仅异步同步pgsync) | 使用逻辑导出/导入，无需超级权限，分钟级延迟 |
| ✅ 双向写入 → 最终一致 | [pglogical](#双向写入最终一致pglogical) | 双主架构，需超级用户安装扩展 |
| ✅ 结构迁移 / 数据导出备份 | `migratectl` + `pgsync` + `cron` | CLI 执行 schema，pgsync 定时同步 |

使用新的 `migratectl` CLI 可以在不同环境下快速执行迁移、校验和重置操作：

```bash
# 初始化或升级 schema
go run ./cmd/migratectl/main.go migrate --dsn "$DB_URL"

# 对比 CN 与 Global 节点结构一致性
go run ./cmd/migratectl/main.go check --cn "$CN_DSN" --global "$GLOBAL_DSN"

## 仅异步同步（pgsync）

当只需要将用户表从主区域异步同步到次区域时，推荐使用 [pgsync](https://github.com/timeplus-io/pgsync)。

1. 初始化基础 schema（默认 `REPLICATION_MODE=pgsync` 不会执行任何 pglogical 步骤）：

   ```bash
   make -C account init-db REPLICATION_MODE=pgsync DB_URL="$SOURCE_DB_URL"
   make -C account init-db REPLICATION_MODE=pgsync DB_URL="$DEST_DB_URL"
   ```

2. 编辑 `account/sql/pgsync.users.example.yaml`，替换源端与目标端 DSN。

3. 使用 pgsync 持续同步，可结合 cron 运行增量同步：

   ```bash
   # 全量初始化
   pgsync --config account/sql/pgsync.users.example.yaml --once

   # 每分钟增量同步
   * * * * * /usr/local/bin/pgsync --config /path/to/pgsync.users.yaml >> /var/log/pgsync.log 2>&1
   ```

4. 如需同步更多表，只需在配置文件中新增表条目。所有表均共享基础 schema，无需超级用户权限。

> 📌 `schema.sql` 中的 `origin_node` 默认值为 `local`。在 pgsync 场景下可以保持默认，或者在迁移脚本中通过 `ALTER TABLE ... ALTER COLUMN origin_node SET DEFAULT 'global'` 为不同区域设置标记。

## 双向写入最终一致（pglogical）

当需要两地同时写入并保持最终一致时，可切换到 pglogical 模式。

1. 确保数据库超级用户已经安装 `pglogical` 扩展。
2. 初始化 schema 并应用 pglogical 默认值补丁：

   ```bash
   make -C account init-db REPLICATION_MODE=pglogical DB_URL="$REGION_DB_URL" \
     DB_ADMIN_USER=postgres DB_ADMIN_PASS=secret
   ```

   该命令依次执行：
   - `sql/schema.sql`：业务基础结构；
   - `sql/schema_pglogical_init.sql`：在 `pglogical` schema 中安装扩展；
   - `sql/schema_pglogical_patch.sql`：将 `origin_node` 默认值绑定到 `pglogical.node_name`。

3. 参考下方模板配置双节点复制：

   ```bash
   make -C account init-pglogical-region \
     REGION_DB_URL="$REGION_DB_URL" \
     NODE_NAME=node_cn \
     NODE_DSN="host=cn-homepage.svc.plus port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}" \
     SUBSCRIPTION_NAME=sub_from_global \
     PROVIDER_DSN="host=global-homepage.svc.plus port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}"
   ```

4. 在另一侧节点重复执行并互为订阅，实现双主写入。

## 🔐 权限与 Schema 设置

- 1️⃣ 授权 pglogical schema 使用权限

pglogical schema 与业务 schema 分离，以防逻辑复制函数污染业务层。

在初始化完成后执行：

bash
复制代码
sudo -u postgres psql -d account -c "GRANT USAGE ON SCHEMA pglogical TO PUBLIC;"
2️⃣ 授权业务用户（app_user）
sql
复制代码
-- 登录 postgres
sudo -u postgres psql -d account

-- 授权 app_user 对 public schema 全权限
ALTER SCHEMA public OWNER TO app_user;
GRANT ALL ON SCHEMA public TO app_user;
GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO app_user;
GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO app_user;
GRANT ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public TO app_user;

-- 授权 pglogical schema 使用权限（仅使用，不可修改）
GRANT USAGE ON SCHEMA pglogical TO app_user;

\q
⚙️ 执行顺序建议

| 步骤 | 节点 | 脚本 / 命令 | 说明 |
| --- | --- | --- | --- |
| 1️⃣ | Global | schema.sql | 创建业务结构（含 version/origin_node） |
| 2️⃣ | CN | schema.sql | 创建相同业务结构 |
| 3️⃣ | Global | schema_pglogical_patch.sql | 绑定 origin_node 默认值到 pglogical |
| 4️⃣ | CN | schema_pglogical_patch.sql | 同上 |
| 5️⃣ | Global | schema_pglogical_region.sql + 参数 | 定义 Global provider + 订阅 CN |
| 6️⃣ | CN | schema_pglogical_region.sql + 参数 | 定义 CN provider + 订阅 Global |

💡 执行 `schema_pglogical_region.sql` 或对应的 `make init-pglogical-region-*` 目标前，请确保连接用户拥有 PostgreSQL 超级用户权限。

### 手动执行模版脚本

使用相同的 `schema_pglogical_region.sql` 模版即可初始化 Global 与 CN 两个节点，只需传入不同的变量：

```bash
# Global 节点示例
psql "$REGION_GLOBAL_DB_URL" -v ON_ERROR_STOP=1 \
  -v NODE_NAME=node_global \
  -v NODE_DSN='host=global-homepage.svc.plus port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}' \
  -v SUBSCRIPTION_NAME=sub_from_cn \
  -v PROVIDER_DSN='host=cn-homepage.svc.plus port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}' \
  -f account/sql/schema_pglogical_region.sql

# CN 节点示例
psql "$REGION_CN_DB_URL" -v ON_ERROR_STOP=1 \
  -v NODE_NAME=node_cn \
  -v NODE_DSN='host=cn-homepage.svc.plus port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}' \
  -v SUBSCRIPTION_NAME=sub_from_global \
  -v PROVIDER_DSN='host=global-homepage.svc.plus port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}' \
  -f account/sql/schema_pglogical_region.sql
```

也可以通过新的 `make init-pglogical-region` 目标自定义变量，例如：

```bash
make init-pglogical-region \
  REGION_DB_URL="$REGION_DB_URL" \
  NODE_NAME=node_example \
  NODE_DSN="host=example port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}" \
  SUBSCRIPTION_NAME=sub_from_peer \
  PROVIDER_DSN="host=peer port=5432 dbname=account user=${PGLOGICAL_USER} password=${PGLOGICAL_PASSWORD}"
```

- 若使用业务账号（如 `app_user`）执行初始化，PostgreSQL 会提示缺少超级用户权限并跳过 `pglogical` 初始化。
- 建议改用 `postgres` 等超级用户连接执行，或由管理员预先安装 `pglogical` 扩展并授予业务用户访问权限。
- 如果扩展已由管理员创建，可直接重新运行 `make init-pglogical-region-cn` 完成复制集与订阅配置。


🧩 验证同步状态
sql
复制代码
SELECT * FROM pglogical.show_subscription_status();
若输出中：

ini
复制代码
status = 'replicating'
即表示双向复制同步正常。

🚀 双向同步特性汇总
特性	实现机制
双主写入	两端都是 Provider + Subscriber
唯一性保障	所有主键为 gen_random_uuid()，避免冲突
邮箱唯一	lower(email) 唯一索引
异步复制	WAL 级逻辑同步，自动断点续传
结构一致性	schema_base_bidirectional_enhanced.sql 保证完全相同
幂等可重建	全部 IF NOT EXISTS，可重复执行
扩展性	可新增字段或表，通过 replication_set_add_table() 同步
冲突检测	version + origin_node 字段支持双写检测与合并

🔍 冲突检测与合并策略
双向同步可能出现两节点同时更新同一行的情况。
可通过 version 与 updated_at 字段进行检测：

sql
复制代码
-- 检查 CN 与 Global 版本不一致的行
SELECT uuid, username, version, updated_at, origin_node
FROM users
WHERE version <> (
  SELECT version FROM dblink('dbname=account_global', 'SELECT version, uuid FROM users')
  AS global_users(uuid uuid, version bigint)
  WHERE global_users.uuid = users.uuid
);

-- 可根据 version 或 updated_at 决定“最后写赢”
推荐策略：

比较 version → 较高者为最终版本；

若版本相同，则以 updated_at 较新的为准；

origin_node 可用于回溯更新来源（CN / Global）。

🧱 Schema 增强说明（相较旧版）
字段	类型	说明
version	BIGINT DEFAULT 0	行级版本号，防止冲突或支持 last-write-wins
origin_node	TEXT DEFAULT current_setting('pglogical.node_name', true)	标识该记录来源节点
bump_version()	trigger function	每次更新自动自增 version
*_bump_version	trigger	自动维护版本号

🧠 附录：生产建议
定期执行结构一致性校验（migratectl check）

对业务字段更新保持幂等逻辑（可多次执行）

为新表自动加入：

uuid 主键（gen_random_uuid()）

created_at, updated_at, version, origin_node

保持两端 PostgreSQL 参数一致：

conf
复制代码
wal_level = logical
max_replication_slots = 10
max_wal_senders = 10
shared_preload_libraries = 'pglogical'
🧩 验证 checklist

 Global 与 CN 节点均执行相同的 schema

 所有表包含 version + origin_node

 pglogical 双向订阅已建立

 status = 'replicating'

 version 字段随 UPDATE 自增
