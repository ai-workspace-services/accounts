# CLI 使用

本仓库包含多个命令行工具：

## 账号服务主程序

二进制名称由 Makefile 设置为 `xcontrol-account`，主入口在 `cmd/accountsvc`。

```bash
xcontrol-account --config config/account.yaml --log-level info
```

参数：
- `--config`：配置文件路径
- `--log-level`：`debug|info|warn|error`

## createadmin（超级管理员）

```bash
go run ./cmd/createadmin/main.go \
  --driver postgres \
  --dsn "$DB_URL" \
  --username Admin \
  --password ChangeMe \
  --email admin@svc.plus
```

常用参数：
- `--driver`：`postgres` 或 `memory`
- `--dsn`：PostgreSQL DSN
- `--groups` / `--permissions`
- `--current-password`：更新已有管理员时必需
- `--mfa`：管理员启用 MFA 时必需

## migratectl（迁移 / 导出 / 导入）

```bash
# 迁移
migratectl migrate --dsn "$DB_URL"

# schema 校验
migratectl verify --dsn "$DB_URL" --schema sql/schema.sql

# 导出/导入
migratectl export --dsn "$DB_URL" --output account-export.yaml
migratectl import --dsn "$DB_URL" --file account-export.yaml

# 根模式比较核心账号字段；-D/-C 分别是 --diff/--dry-run 的简写
migratectl -D -C \
  --source-dsn-env MIGRATECTL_SOURCE_DSN \
  --target-dsn-env MIGRATECTL_TARGET_DSN \
  --environment prod --writers-paused \
  --schema-sha256 "$ACCOUNTS_SCHEMA_SHA256" \
  --billing-schema-sha256 "$BILLING_SCHEMA_SHA256" \
  -o markdown
```

根模式 `--diff/-D` 与 `--dry-run/-C` 都是只读核心账号比较入口。`compare-core-users -o raw` 保留现有 JSON receipt（默认）；`-o yaml` 输出同一 receipt 的 YAML；`-o markdown` 输出不含用户标识及摘要值的聚合差异表。比较发现差异时仍返回非零退出码，并先写出所选格式的安全 diff。差异只按数量报告：email、Proxy UUID、密码哈希分别统计不一致数及 source-only / target-only 用户数。

For one explicitly reviewed database upgrade, use `controlled-migrate`. It
reads only the checked-in embedded `sql/migrations/*.up.sql` files, selects one
file by `--target-version`, requires its exact SHA-256, and reads its DSN only
from `ACCOUNTS_MIGRATION_DSN`. It requires the exact current version and the
immediate next checked-in migration; it never forces dirty state, skips steps,
or downgrades. See [the controlled migration runbook](../migrations/controlled-migrate.md).

migratectl reset is disabled because it would remove retained account and
billing history. Use forward-only migratectl migrate upgrades. sql/schema.sql
is a non-destructive bootstrap baseline; it never drops existing tables.

Snapshot imports protect existing account profiles. Merge imports skip existing
profiles and never replace their attributes or proxy UUIDs. A non-merge replace
import is accepted only when the target users table is empty. Username/email
conflicts and existing-user UUID rekeys abort before any writes.

## syncctl（跨环境同步）

```bash
syncctl --config config/sync.yaml push
syncctl --config config/sync.yaml pull
syncctl --config config/sync.yaml mirror
```

## Makefile 快捷命令

```bash
make build
make start
make create-super-admin
make account-export
make account-import
```

相关脚本位于 `scripts/`。
