# Stripe 计费 P1.5:欠费执行面(suspend 断流 + 恢复路径)

> **Status**: ⏳ 代码完成 + 全量测试通过,待合并部署
> **Date**: 2026-07-12
> **Related PRs**: 本仓分支 `feat/stripe-billing-p1`(commit `841f20c`,叠加在 P1 之上);billing-service 分支 `feat/stripe-billing-p1.5`(commit `2b9f4ef`)
> **设计文档**: billing-service `docs/stripe-billing-integration-plan.md` §1.5 / §4 P1.5

## 背景

P1 完成后欠费用户只会被标 `arrears/throttled`(纯 DB 状态),没有组件把它落到 xray 层 —— "欠费→执行"缺最后一公里。按规划 P1.5 用现成 agent sync 通道补齐,不引入新组件。

## 实现内容(本仓)

- **`arrears_since` 欠费起点列**:`sql/20260712_arrears_since.sql` 迁移 + `applyBillingSchema` 幂等 DDL(先 CREATE TABLE IF NOT EXISTS 兜底再 ALTER,避免新库启动失败)。`markAccountArrears` 首次置位、同一欠费期内重复 payment_failed **不推进时钟**;`resetQuotaForPlan`(invoice.paid / 订阅激活)清零
- **agent sync 断流**:`listAgentUsers` + `internalNetworkIdentities` 批量查 `ListSuspendedAccountUUIDs()`(单查询,非 per-user),排除 `suspend_state='suspended'` 账号 → agent 下次 sync 从 xray 配置移除 client(分钟级断流)+ 停止计量归属。**sandbox 演示账号豁免**(Guest 体验不依赖计费状态;若在 identities 排除会产生无法归属的流量)
- **手动清欠恢复**:`POST /api/auth/admin/billing/accounts/:accountUUID/clear-arrears`(admin.settings.write 权限)——清 arrears/arrears_since、throttle→normal、suspend→active,发布 `arrears_cleared` PGMQ 事件;自动恢复路径(invoice.paid → resetQuotaForPlan)P1 已有,本次确认覆盖 suspend_state
- **测试**:suspend 断流(agent users + identities 双端点)、admin 清欠(含 404/401)、欠费期时钟保持;`go test ./...` 全过

## billing-service 侧(配套,分支 feat/stripe-billing-p1.5)

- `SuspendSyncer` 后台任务:按 `ARREARS_SWEEP_INTERVAL`(默认 1h)扫描 arrears 未 suspended 账号,`arrears_since` 超过 `ARREARS_SUSPEND_THRESHOLD`(默认 14d,规划决策 #3)→ `suspend_state='suspended'`
- 评率主循环:余额转负时置 `arrears_since`,恢复正数时清零
- 无 `arrears_since` 的存量 arrears 行不猜测、不升级(留待下一次欠费期或人工处理)

## 设计要点

- **字段所有权**:`suspend_state` 的 suspended 置位归 billing-service(时间驱动);active 恢复归 accounts(invoice.paid / 手动清欠)。与规划 §6 "共享 PG 双写方"约定一致
- throttle 真限速依旧不做(xray 不原生支持),throttled 仅作预警状态
- 断流走现成 agent sync 通道,零新组件、零新端口

## 遗留待办

- [ ] 两分支合并部署(先 accounts 后 billing-service,列先行、扫描后启)
- [ ] console:arrears/throttled/suspended 状态提示 + 催缴邮件(规划 P1.5 最后一项,未在本次范围)
- [ ] P2:7 天低用量退款、升降级 proration、console 定价页
