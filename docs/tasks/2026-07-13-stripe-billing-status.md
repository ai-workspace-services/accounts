# Stripe 订阅 / 账单 / 计费打通 —— 进度与现状(交接快照)

> **Status**: P0 ✅ 已上线(sandbox)· P1 ✅ 已合并部署 · P1.5 🟡 代码就绪待合并 · P2/P3 ⬜ 未开工
> **Date**: 2026-07-13
> **本文用途**: 给接替开发的 agent 一份权威现状,避免重读整条 PR 链。总体设计仍以 billing-service `docs/stripe-billing-integration-plan.md` 为准,本文只记"到 2026-07-13 为止做到哪、下一步从哪接"。
> **跨仓**: accounts(`ai-workspace-services/accounts`,P0/P1 落点)+ billing-service(`ai-workspace-services/billing-service`,规划源 + P1.5 dunning + FinOps)+ playbooks(`ai-workspace-infra/playbooks` → 合入 `x-evor/playbooks`,部署)

## 0. 一句话现状

线上 accounts.svc.plus 的 Stripe 计费**已在 sandbox 模式激活并跑通**:密钥经 Vault OIDC 注入,webhook 端点验签生效,P1 的套餐目录 + entitlement sync 已合并部署。剩下的是把 P1.5(欠费→停用执行面)合并,以及 P2/P3(退款/proration/对账/console)开工。

## 1. 分期进度总表

| 阶段 | 内容 | 状态 | 落点 |
|---|---|---|---|
| **P0** | prod Stripe 密钥接线(Vault→CI→app.env) | ✅ 上线(sandbox) | accounts [#18](https://github.com/ai-workspace-services/accounts/pull/18)[MERGED] + [#20](https://github.com/ai-workspace-services/accounts/pull/20)[MERGED] + [#22](https://github.com/ai-workspace-services/accounts/pull/22)[MERGED];playbooks [#121](https://github.com/ai-workspace-infra/playbooks/pull/121)[MERGED] |
| **P1** | 套餐目录 + webhook 审计去重 + entitlement sync | ✅ 已合并部署 | accounts [#19](https://github.com/ai-workspace-services/accounts/pull/19)[MERGED] |
| **P1.5** | 欠费 14 天 → suspended + agent-sync 断流 | 🟡 代码就绪,**待开 PR + 合并** | 分支 `feat/stripe-billing-p1.5`(accounts + billing-service 各一份) |
| **P2** | 7天低用量退款 + 升降级 proration + console 定价页 + 注销联动 | ⬜ 未开工 | — |
| **P3** | reconcile-stripe 对账 job + `/v1/usage/window` + 指标 | ⬜ 未开工 | billing-service 侧 |

## 2. P0 —— 已上线,如何验证它是活的

**现状证据**(任何 agent 可复现):
```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  https://accounts.svc.plus/api/billing/stripe/webhook \
  -H "Content-Type: application/json" -d '{}'
# 返回 401 invalid_signature  → 密钥已装配(若未配置会返回 stripe_not_configured)
```

### 2.1 as-built 密钥链路(最终形态,#22 后)

```
Stripe Dashboard(sandbox)
   └─ sk_test_* / whsec_*  ← 人工,只经操作者终端
        ▼
Vault kv/billing-service   ← 唯一源头。字段:SANDBOX_STRIPE_SECRET_KEY / SANDBOX_STRIPE_WEBHOOK_SECRET
   │                          (PROD_STRIPE_* 两个字段预留,上 live 时填)
   │  CI OIDC 读(role github-actions-accounts,已授 kv/data/billing-service 读权限)
   ▼
pipeline.yml「Load Vault secrets」→「Resolve Deploy Secret Source」
   │  韧性:Vault 读失败(continue-on-error)→ 回退 GH secret 镜像 → 再空(端点关闭)
   │  模式:仓库变量 STRIPE_MODE(= sandbox)按整对选 SANDBOX_* / PROD_*(sk 与 whsec 永不混模式)
   ▼
playbooks accounts_service role(app.env.j2 + target.yml lineinfile)
   ▼
install.svc.plus:/opt/cloud-neutral/accounts/managed/prod/env/app.env → 容器 os.Getenv
```

要点:
- **密钥现在从 Vault OIDC 直读,不再需要手工 `vault kv get | gh secret set` 同步**。GH secrets(`SANDBOX_STRIPE_*`)仅作 Vault 故障期 fallback,当前未设(纯 OIDC 路径)。
- `STRIPE_MODE` 仓库变量当前 = `sandbox`。切 prod 只需填 Vault 的 `PROD_STRIPE_*` 两字段 + `gh variable set STRIPE_MODE -b prod` + 重跑 deploy,**不改代码**(SOP 见 §6)。
- Vault role `github-actions-accounts` 的 policy 已加 `kv/data/billing-service` 读权限(provisioning 命令见仓库根 `README.md`「CI/CD 部署前置条件」)。

### 2.2 PR #20 → #21 → #22 的来龙去脉(避免接替者困惑)

- **#20**:把 Stripe 密钥从「手工同步 GH secret」改为「CI OIDC 直读 kv/billing-service」。合并后首次 deploy(run 29216765565)因 policy 尚未授 billing-service 读权限而 **403 失败** → 补授 policy 后恢复。
- **#21**(已 CLOSED 不合并):想加「Vault 故障不阻塞发版」的韧性,但含三处回归 —— ①runtime token 铸造引用了不存在的 `steps.vault.outputs.token` 导致每次静默跳过(32 天后会复发 stale-token 事故);②整体回退了 #20 的 Stripe-via-Vault;③误删 P1 代码/docs。
- **#22**(已 MERGED):保留 #21 的韧性目标,修掉三处回归。最终形态:`Load Vault secrets` 可失败(continue-on-error),单独的 `Resolve Deploy/Validate Secret Source` 步骤在 Vault 值与 GH 镜像间择一;runtime token 铸造改用 `outputToken: true` + `steps.vault.outputs.vault_token`;Vault 鉴权不可用→告警保留主机现有 token,鉴权 OK 但 create-orphan 被拒→仍大声失败。

> ⚠️ 接替者注意:runtime Vault token(主机 `XWORKMATE_VAULT_TOKEN`)每次 deploy 自动铸造(orphan periodic,period 768h)。**部署间隔超 32 天 token 会过期**,需手动触发一次 workflow_dispatch 续期。

## 3. P1 —— 已合并部署(#19)

代码在 main:`api/billing_plans.go`、`api/entitlements.go`、`api/stripe.go`、`internal/store/billing*.go`;schema 表 `billing_plans`、`stripe_webhook_events`(`applyBillingSchema` 启动幂等建表 + 种子 `TRIAL-7D` / `FREE`)。

已生效能力(细节见同目录 [`2026-07-11-stripe-billing-p1.md`](2026-07-11-stripe-billing-p1.md)):
- **套餐目录数据驱动**:`GET /api/billing/plans`(公开,定价页用)+ admin CRUD;checkout 校验优先读目录,目录空时回退 `STRIPE_ALLOWED_PRICE_IDS`。
- **webhook 审计去重**:先落 `stripe_webhook_events` 再处理,重放零副作用。
- **entitlement sync**(webhook 驱动,`syncSubscriptionEntitlements` / `applyPlanEntitlements` / `resetQuotaForPlan` / `markAccountArrears`):
  - `subscription.created/updated` → 写 `account_billing_profiles`,created 重置配额,非 trial 计划标存量 trial 为 `superseded`
  - `invoice.paid` → 重置配额 + 清 arrears
  - `invoice.payment_failed` → `arrears=true`(梯度升级归 P1.5)
  - `subscription.deleted` → 降 FREE + 清零配额
- **PGMQ `billing_events` 队列**:accounts 在各 sync 点位发布生命周期事件;扩展缺失时优雅降级为 no-op。消费方(billing-service)后续接。

**P1 遗留(接替者可直接做)**:
- [ ] 运营在 admin 后台录入首批**付费**套餐(plan_id ↔ stripe_price_id);目前只有 TRIAL-7D / FREE 种子。
- [ ] Stripe Dashboard 建 Products/Prices(sandbox 已有 webhook + 密钥;付费 price 尚未建)。

## 4. P1.5 —— 代码就绪,待合并(下一个动作)

**分支 `feat/stripe-billing-p1.5` 已存在于两仓,尚未开 PR / 未合并 main。**

- **billing-service** `feat/stripe-billing-p1.5`(2 commits):`feat(dunning): P1.5 arrears grace sweep -> suspend_state=suspended` + 任务归档 doc。定时扫描 arrears 持续超阈值(14 天,配置化)的账户 → 迁移 `account_quota_states.suspend_state='suspended'`。
- **accounts** `feat/stripe-billing-p1.5`:`listAgentUsers` / `internalNetworkIdentities` 过滤 `suspend_state='suspended'` 账号(join quota_states)→ agent 下次 sync 断流;恢复路径 `invoice.paid`/手动清欠 → `suspend_state='active'`。
  - 注:main 上 `api.go` 已有登录态拦截(`account_suspended`,api.go:~1548),但**agent-sync 断流过滤在分支上、未进 main**。

**接替者第一步 = 把这两个分支各开 PR、跑测试、合并部署**,并在 Stripe Dashboard 用 test clock 验证 payment_failed → 14 天 → suspended → 节点断流的全链路。

## 5. P2 / P3 —— 未开工(设计已定,直接按规划执行)

细节见 billing-service `docs/stripe-billing-integration-plan.md` §4:
- **P2**:`POST /api/auth/subscriptions/refund`(≤7天 && 窗口内用量<5% → Stripe refund + 降级 + ledger 冲正)· `POST /api/auth/subscriptions/change`(proration=create_prorations)· console 定价页接 `/api/billing/plans` · 注销 30 天冷静期联动。
- **P3**(billing-service):`POST /v1/jobs/reconcile-stripe` 对账 drift 报告 · `GET /v1/usage/window` 内部端点(供退款判定)· webhook 失败率/entitlement 滞后/arrears 账户数指标接 VictoriaMetrics。

## 6. 切 prod(Live)SOP —— 备用

1. Stripe Dashboard 切 **Live mode** → 拿 `sk_live_*`;建正式 webhook(URL 同 sandbox:`https://accounts.svc.plus/api/billing/stripe/webhook`,勾同样 6 事件)→ `whsec_*`
2. `vault kv patch kv/billing-service PROD_STRIPE_SECRET_KEY='sk_live_…' PROD_STRIPE_WEBHOOK_SECRET='whsec_…'`
3. `gh variable set STRIPE_MODE -R ai-workspace-services/accounts -b prod`
4. 重跑 deploy(或推一次 main)→ CI OIDC 自动读 PROD_* 对
5. 回滚 = `STRIPE_MODE` 改回 `sandbox` + 重跑 deploy

## 7. 验证 runbook(每次改动后)

1. **env 落盘**(只数行,不回显值):`ssh root@install.svc.plus 'grep -c "^STRIPE" /opt/cloud-neutral/accounts/managed/prod/env/app.env'` → 期待 3
2. **端点活性**:§2 的 curl → `401 invalid_signature`(非 `stripe_not_configured`)
3. **webhook 配对**:Stripe Dashboard → endpoint → Send test event → HTTP 200
4. **全流程**:console.svc.plus 登录 → checkout → 测试卡 `4242 4242 4242 4242` → 回跳后 `GET /api/auth/subscriptions` 出现 `provider=stripe`;核对 `account_billing_profiles` / `account_quota_states` 已按目录写入

## 8. 事件契约(Dashboard 勾选,与代码 switch 一一对应)

`checkout.session.completed` · `customer.subscription.created` · `customer.subscription.updated` · `customer.subscription.deleted` · **`invoice.paid`**(**不是** `invoice.payment_succeeded` —— 代码只认 `invoice.paid`,且它是超集覆盖 $0 发票/余额抵扣/带外支付)· `invoice.payment_failed`。密钥接线完整 runbook 见仓库 `docs/STRIPE_BILLING_SETUP.md`(若已合并)。

## 9. 关键文件速查

| 关注点 | 位置 |
|---|---|
| 总体规划(权威) | billing-service `docs/stripe-billing-integration-plan.md` |
| P1 实现细节 | accounts `docs/tasks/2026-07-11-stripe-billing-p1.md` |
| entitlement sync | accounts `api/entitlements.go` · `api/stripe.go` |
| 套餐目录 store | accounts `internal/store/billing*.go` |
| CI 密钥/token 铸造 | accounts `.github/workflows/pipeline.yml`(Load Vault secrets / Resolve Secret Source / Mint Runtime Vault Token) |
| Vault role provisioning | accounts `README.md`「CI/CD 部署前置条件」 |
| P1.5 dunning | billing-service 分支 `feat/stripe-billing-p1.5` |
