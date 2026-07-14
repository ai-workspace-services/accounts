# Overlay 控制面(设备/节点注册、配置下发、ack)+ accounting 更新

> **Status**: ✅ 已合并(重接到 main)
> **Date**: 2026-07-14(实现)· 2026-07-22(重接落地)
> **Related PRs**: 原 [#29](https://github.com/ai-workspace-services/accounts/pull/29)(分支 `feat/overlay-control-plane`)因分支基础问题未直接合并,见下方说明;功能经重接 PR 落地。

## 问题:分支建在已废弃的 PR #21 提交上

`feat/overlay-control-plane` 只有 2 个提交:
1. `771987b "ci: document billing-service Vault access"` —— 这正是**已关闭、从未合并的 PR #21** 的提交(该 PR 因回退了 #20 的 Stripe-via-Vault + 误删 P1 代码 `entitlements.go`/`billing_events.go`/`billing_p1_test.go` 而被 #22 取代关闭)。
2. `4d3a9ab "feat: implement overlay control plane and accounting updates"` —— 真正的 overlay 功能实现,4464 行新增,建立在被删文件之上。

结果:PR #29 与 main 的 diff 里混入了大量"文件被删"的假冲突,且 `mergeable: CONFLICTING`。

## 修复:只挑真正的功能提交

Cherry-pick `4d3a9ab`(跳过 `771987b`)到当前 main,解决 8 处真实冲突。冲突多数是**独立重复实现**(overlay 分支在写这部分时,`main` 尚未有对应功能,后来 Stripe P1.5(#30/#31)和 CI 环境路由(#22/#27)先合并了):

| 冲突文件 | 处理 |
|---|---|
| `.github/workflows/pipeline.yml`、`scripts/github-actions/resolve-pipeline-flags.sh` | 保留 main 现状(#27 已合并且生产验证过);overlay 分支独立实现了几乎相同的 env 路由,但 `push.branches` 漏了 `main`,是不完整版本 |
| `api/internal_network_identities.go`、`api/stripe.go`、`internal/store/store.go`、`internal/store/accounting_memory.go`、`internal/store/accounting_postgres.go` | 保留 main 现状(P1.5 的 `ListSuspendedAccountUUIDs` + `UpsertAccountQuotaState`,已合并测试);overlay 分支独立实现了 `MarkAccountArrears`/`ClearAccountArrears`/`IsAccountSuspended`,功能重叠但更简陋(逐用户查询、无 sandbox 豁免),整体丢弃 |
| `docs/tasks/README.md` | 合并双方索引行 |

Overlay 功能本体(未冲突,原样保留):`api/overlay.go`、`api/internal_overlay.go`、`cmd/overlayctl/`(CLI + 测试)、`internal/store/overlay_memory.go`、`internal/store/overlay_postgres.go`、`sql/20260601_overlay_control_plane.sql`、`docs/overlay-config-contract.md`。

## 测试

`go build && go vet && go test ./...` 全绿。

## 遗留待办

- 无(功能完整落地;PR #29 本身应关闭,内容已通过本次重接 PR 进入 main)。
