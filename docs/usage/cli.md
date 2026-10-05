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
```

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
