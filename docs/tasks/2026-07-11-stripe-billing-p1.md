# Stripe 计费 P1:套餐目录 + webhook 审计 + entitlement sync

> **Status**: ✅ 已合并部署(2026-07-12)
> **Date**: 2026-07-11(实现)· 2026-07-12(合并)
> **Related PRs**: accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19) [MERGED];前置 P0 = accounts [#18](https://github.com/ai-workspace-services/accounts/pull/18) + playbooks [#121](https://github.com/ai-workspace-infra/playbooks/pull/121) [MERGED]
> **最新现状**: 见 [`2026-07-13-stripe-billing-status.md`](2026-07-13-stripe-billing-status.md)(跨仓交接快照)
> **设计文档**: billing-service `docs/stripe-billing-integration-plan.md` §4 P1

## 实现内容

- **`billing_plans` 套餐目录**(数据驱动核心):store 模型/接口 + memory/postgres 实现;`applyBillingSchema` 启动时幂等建表;种子 `TRIAL-7D`(10GiB/7天)+ `FREE`(仅缺席时插入,不覆盖运营修改)
- **`stripe_webhook_events` 审计/去重**:webhook 先落事件再处理;重放已处理事件 → `{"received":true,"duplicate":true}` 零副作用;失败留 `failed`+`last_error` 可重放
- **entitlement sync**(`api/entitlements.go`):
  - `subscription.created/updated`(active/trialing)→ 按目录写 `account_billing_profiles`(package/quota/倍率,`pricing_rule_version=plan:<id>`);created 同时重置 `account_quota_states`;非 trial 计划将存量 trial 订阅标 `superseded`
  - `invoice.paid` → 重置配额 + 清 arrears(续期)
  - `invoice.payment_failed` → `arrears=true`(梯度升级归 billing-service P1.5)
  - `subscription.deleted` → 降级 FREE 档案 + 清零配额
  - OAuth 首登 trial → 同步应用 TRIAL-7D 目录权益
- **checkout 校验读目录**:目录有带价 active 计划时目录为准;目录为空时回退 `STRIPE_ALLOWED_PRICE_IDS`(bootstrap 模式)
- **API**:公开 `GET /api/billing/plans`(active only,定价页用);admin `GET/PUT/DELETE /api/auth/admin/billing/plans[/:planId]`(复用 admin.settings.read/write 权限)
- **测试**:签名 webhook 全链路(created 同步+去重重放防篡改、deleted 降级)、arrears/重置单测、目录校验优先级、公开/管理端点、OAuth trial 权益;`go test ./...` 7 包全过

## 增补(同分支第二提交):PGMQ 事件队列 + Vault 路径

- **PGMQ `billing_events` 队列**(扩展 pgmq v1.8.0,postgresql.svc.plus 运行时镜像内置,线上 accounts 库 available 未安装):accounts 在 entitlement sync 各点位发布紧凑生命周期事件(`subscription_activated/updated`、`invoice_paid`、`payment_failed`、`subscription_deleted`、`trial_provisioned`)。启动时 `EnsureBillingEventQueue`:检测/尝试 `CREATE EXTENSION pgmq` + `pgmq.create('billing_events')`,失败则**优雅降级为 no-op**(日志提示 operator 以 superuser 建扩展)。发布 best-effort,webhook 流程绝不因队列失败而失败;去重重放不重复发布(有测试断言)。消费方(billing-service reconcile/催缴/通知)后续接,accounts 不感知。
- **Vault 路径规划**:Stripe 密钥归 **`kv/billing-service`**(`STRIPE_SECRET_KEY`、`STRIPE_WEBHOOK_SECRET`),与 accounts 的 OAuth 密钥(kv/accounts.svc.plus)分域;GH secrets 名不变。

## 设计要点

- 保留既有 profile 的 `base_price_per_byte`(目录暂不管每字节单价,billing-service 默认价适用)
- 事件无 `id` 时(理论不发生)跳过去重照常处理
- kind 白名单 `trial|subscription|paygo_topup`(paygo 表结构预留,P2+)

## 遗留待办

- [ ] 合并部署(applyBillingSchema 自动建表 + 种子)
- [ ] 运营在 admin 后台录入首批付费套餐(plan_id ↔ stripe_price_id)
- [ ] Stripe Dashboard 建 Products/Prices + webhook,secrets 入 Vault/GH(P0 已接线,值未配)
- [ ] P1.5(billing-service):arrears 14 天 → suspended + listAgentUsers/identities 过滤
- [ ] P2:7 天低用量退款、升降级 proration、console 定价页接 /api/billing/plans
