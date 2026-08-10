# UAT 共享计费 Schema 初始化补全

> **Status**: 🟡 已部署 UAT；结构验收通过，真实流量验收待环境解阻
> **Date**: 2026-08-01
> **Related PRs**: [accounts #46](https://github.com/ai-workspace-services/accounts/pull/46) [MERGED] · [portal #131](https://github.com/ai-workspace-services/portal/pull/131) [MERGED] · [gitops #130](https://github.com/ai-workspace-infra/gitops/pull/130) [MERGED]

## 目标与范围

修复 UAT 的共享 PostgreSQL 只初始化了部分计费表的问题。Accounts 与
Billing 继续使用**同一个** `web-saas` PostgreSQL；本任务只让 Accounts
启动时以幂等、非破坏性的方式补齐
`sql/20260401_accounting_control_plane.sql` 中 Xray → Exporter → Billing →
PostgreSQL → Accounts → Portal 所需的 control-plane 表和索引。

不拆库、不新增服务、不直接修改 UAT 数据库，也不改 Portal UI、GitOps、
Vault、运行主机或 `svc.plus` 生产环境。

## 生产可观测性基准（只读参考）

生产 Grafana 的 [Xray dashboard](https://observability.svc.plus/grafana/d/begqoward2epsf/xray-dashboard?orgId=1&from=now-30d&to=now&timezone=browser&var-job=$__all&var-instance=$__all&var-user=$__all)
展示了本轮 UAT 需要最终对齐的使用量形态：按用户查看下载/上传速率，以及
下载/上传的 30 天累计流量。它是 Observability 的展示基准，**不是 Billing
的写入依赖或计费真相来源**。

- Billing/共享 PostgreSQL 的规范聚合键仍为 `account_uuid`；多 Xray 节点和
  多 inbound 的数据按同一 UUID 汇总。
- 邮箱仅可作为 Portal 展示或 Grafana 可观测性标签，不能作为计费聚合键：邮箱
  可修改、可能为空，且不应被复制进账单主键。
- UAT 通过 minute bucket 同时保留 `uplink_bytes`、`downlink_bytes` 与
  `total_bytes`，从而能复现上述速率/累计展示，同时以 `total_bytes` 进行订阅
  配额扣减。
- Exporter 应向 Billing 和 Observability 扇出；即使 Grafana 暂不可用，Billing
  也必须能继续落库和聚合。

## UAT 只读证据

- `web-saas` 容器当前为 `daily-build-2026.08.01-r1`。
- `account_quota_states` 已存在，且 `period_start` / `period_end` 已生效。
- PostgreSQL 对下列表报 `relation does not exist`：
  `traffic_minute_buckets`、`billing_ledger`、
  `account_policy_snapshots`、`node_health_snapshots`。
- Billing 还记录了 `account_user` 密码认证失败；这是环境/Vault 凭据事件，
  **不属于本次代码修复范围**，不处理或变更任何 secret。
- Agent-proxy 是 systemd 服务而非 Docker：`agent-svc-plus`、`xray`、
  `xray-tcp` 和两个 exporter 均为 active，Billing collect job 每分钟成功。
  exporter 提示 `/var/log/xray/access.log` 缺失。
- 当前运行同时存在 XHTTP 与 TCP 服务，和目标 UAT 单 source / 单 inbound
  配置不一致；作为独立的配置验收项记录，本 PR 不改变该配置。

## 本次改动

- 扩展 `applyBillingSchema`：启动时创建完整共享 accounting control-plane：
  checkpoint、minute bucket、ledger、quota/profile、source sync、policy、
  node health、scheduler decision 及其查询索引。
- 保留已有 `billing_plans`、Stripe webhook 与 quota period 的 bootstrap。
- 全部 DDL 使用 `CREATE ... IF NOT EXISTS` 或 `ADD COLUMN IF NOT EXISTS`；
  没有删除、截断或数据迁移操作。
- 添加纯单元契约测试，锁定需要的表、索引、period 列和非破坏性约束。

## 验收查询（部署后由 UAT 运维执行）

```sql
SELECT to_regclass('public.traffic_minute_buckets'),
       to_regclass('public.billing_ledger'),
       to_regclass('public.account_policy_snapshots'),
       to_regclass('public.node_health_snapshots');

SELECT column_name
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = 'account_quota_states'
  AND column_name IN ('period_start', 'period_end');

SELECT indexname
FROM pg_indexes
WHERE schemaname = 'public'
  AND tablename IN ('traffic_minute_buckets', 'billing_ledger',
                    'node_health_snapshots', 'scheduler_decisions');
```

预期：四个 `to_regclass` 均非空、两个 period 字段存在、四个查询索引存在。
随后以真实 Xray 样本验证 minute bucket / ledger 写入以及 Accounts usage
summary、Portal 配额卡读取；凭据认证事件与 XHTTP/TCP 双服务配置另行验收。

## 2026-08-01 UAT 部署结果

GitOps #130 已合并，Doco-CD 已成功收敛到以下不可变镜像：

- Accounts：`daily-build-2026.08.01-r2`（共享 schema bootstrap）。
- Console：`daily-build-2026.08.01-r2`（`/panel/account` 配额与账号面板）。
- Billing：`daily-build-2026.08.01-r1`（既有 UUID 多 inbound 聚合修复）。

结构验收已通过：八张 control-plane 表和 `period_start` / `period_end` 均存在；
`console-uat` 和 `accounts-uat` 的 ping 均为 HTTP 200，未登录 usage summary
正确返回 HTTP 401。

数据面验收尚未通过，当前所有表均为 0 行（checkpoint、minute bucket、ledger、
node health、source sync）。这不能证明聚合和扣费正确，也不能证明 Portal 在真实
账号会话下可展示配额。

### 下一次验收的前置条件

1. UAT/Vault 所有者修复 `account_user` 对共享 PostgreSQL 的认证失败；Billing
   restart 后不应再出现 SQLSTATE `28P01`。
2. 将 agent-proxy UAT 配置收敛到已约定的单 source / 单 inbound，或明确登记
   当前 XHTTP + TCP 双服务为多 inbound 测试拓扑；同时修复 Exporter 的 access log
   路径/用户指标来源。
3. 使用专门的 UAT 测试账号和已知 UUID 产生可撤销的 VLESS 流量，然后依次确认：
   checkpoint、minute bucket、ledger、source sync 都有记录；认证后的
   `/api/account/usage/summary` 返回 period/used/quota 字段；`/panel/account`
   显示对应月度配额卡。不得使用 `svc.plus` 生产账号或流量进行此验证。

## 阻塞项

`account_user` 密码认证失败必须由 UAT 环境/Vault 所有者调查和轮换（如需要）。
本分支不能也不会读取、修改或回显该凭据。其次，当前未产生可验证的 UAT 流量，
需要先建立专用测试样本，不能将生产 Grafana 数据回灌到计费库。
