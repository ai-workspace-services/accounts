# 套餐与月度配额目录契约

状态：P0-Q1 implementation PR，待审阅；未合并、未发布。

## 单一事实源

Supabase/PostgreSQL 的 `billing_plans` 是套餐目录与套餐权益定义的业务事实源。内部 `plan_id` 是套餐标识；`stripe_price_id` 只映射外部 Stripe Price，不用于识别本地套餐、账号或订阅。Stripe 不拥有套餐额度、权益、账号分组或归档状态。

accounts PR #176 已把 `FREE`（5 GiB/月）、`PLUS`（20 GiB/月）和 `UNLIMITED-BETA`（仅内测）作为可重复执行的目录 upsert 加入 `sql/migrations/2026092701_quota_plan_groups.up.sql`。迁移只写套餐目录，不更新或回填用户、订阅、付款、退款、用量账本、账号配额档案或分组成员；`ON CONFLICT (plan_id) DO UPDATE` 让同一 migration 可安全重跑，且不覆盖 provider price 字段。

本 P0-Q1 改动复用该目录和 schema，不增加 SQL schema migration。测试会锁定已有迁移为事务式、按 `plan_id` 幂等、且不触及用户/账号权益记录。没有 schema 变更就不需要新迁移文件。

## 注册与现有账号

- 新密码注册和 OAuth 首次创建账号时，根据 `FREE` 目录记录创建 Free 5 GiB/月的套餐档案与本期额度；不生成 Stripe 订阅，也不发试用订阅。
- usage/billing GET 是只读：没有套餐档案的既有账号继续保持未分配，不在读请求中写入 `FREE`。套餐管理分配仍由管理员显式完成。
- 历史档案若只有 `package_name=default` 且无法解析出本地 `plan_id`，同样作为未分配展示；原有配额档案保留，不会被映射或覆盖成 Free。
- 默认 Free 目录可作为展示参考返回，但 API 标注 `assigned=false`、`planAssignmentStatus=unassigned`；Portal 不可把参考套餐画成该账号已生效的权益。
- 本改动不修改已注册用户的分组、套餐档案、订阅、代理凭据、active 状态、付款或使用记录，也不增加批量分组管理（P2-2）。

## API

`GET /api/billing/plans` 保留 `includedQuotaBytes`，并新增等值的 `maxTrafficBytes`，以明确表示目录中每档套餐的最大流量。无限制内测档通过 `features.fast_lane.mode=unlimited` 表示，字节值 `0` 不应被展示成“0 B 额度”。

`GET /api/account/usage/summary` 和 `GET /api/account/billing/summary` 返回：

- `currentPlan`：已分配时的套餐信息；`maxTrafficBytes` 是当前账号实际生效的档案额度，`catalogMaxTrafficBytes` 是当前目录模板额度。
- `defaultPlan`：Free 目录参考信息，`assigned=false`，不创建任何账号档案。
- `planAssignmentStatus`：`assigned` 或 `unassigned`。

`source` 字段区分 `account_entitlement`、`account_billing_profile` 与 `local_catalog_default`，避免把 Stripe 外部字段误当作套餐来源。

## 与 roadmap 对齐

本契约落实 `feature-subscription-billing-operations/01-plan-catalog.md` 的本地目录、Free/Plus 权益和 Stripe 外部映射边界；沿用 `02-metering-and-entitlements.md` 的月度字节配额以及 `07-existing-user-migration.md` 的“不在未经确认时回填存量账号”原则。该 feature 不执行生产数据迁移或任何环境部署。
