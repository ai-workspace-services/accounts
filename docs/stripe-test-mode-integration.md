# Stripe Test Mode 对接与验收记录

## 适用范围

本文记录 XWorkmate 在 Stripe Test Mode 中的配置和验收流程，覆盖：

`Free（限量） → Pro 月付/年付 → 按需定制服务`

订阅退款、到期或取消后，账户统一降级为限量 Free 用户。

## 安全集成链路

```text
Portal（只提交 planId / public stripePriceId）
  ↓ BFF 转发当前登录会话
Accounts 服务端（Vault 注入 Stripe Secret Key）
  ↓ 创建 Checkout Session
Stripe
  ↓ Webhook + 签名验证
Accounts 更新订阅、配额、退款和 Free 降级
```

Portal 不保存 `sk_test_`、`sk_live_` 或 `whsec_`，也不直接调用 Stripe Secret API。Accounts
必须以套餐目录为准校验 `planId`、`stripePriceId`、套餐类型和支付模式；浏览器传入的其他
Stripe 参数不会被 BFF 转发。

## Stripe 基础配置

Stripe 账户：`acct_1SvuMHLIhVa2N0n8`

操作入口：[Stripe Test Dashboard](https://dashboard.stripe.com/acct_1SvuMHLIhVa2N0n8/test/dashboard)

### 产品和价格

在 **Product catalog → Products** 中确认 Pro 产品，并创建两个 recurring Price：

| 方案 | Stripe recurring interval | 应用套餐 |
| --- | --- | --- |
| Pro 月付 | month | `PRO-MONTHLY` |
| Pro 年付 | year | `PRO-YEARLY` |

将 Price ID 配置到 Accounts：

```text
STRIPE_PRICE_PRO_MONTHLY=price_xxx
STRIPE_PRICE_PRO_YEARLY=price_xxx
```

不要创建或启用 Trial Price，也不要设置 `trial_period_days`。`FREE` 不需要 Stripe Price。

### API 密钥

在 **Developers → API keys → Test mode** 配置：

```text
STRIPE_SECRET_KEY=sk_test_xxx
STRIPE_PUBLISHABLE_KEY=pk_test_xxx
```

`sk_test_xxx` 只能配置在 Accounts 服务端，不能进入 Portal 或浏览器代码。

## Webhook 配置

在 **Developers → Webhooks → Add endpoint** 添加 UAT Accounts endpoint：

```text
https://accounts-serverless-uat.onwalk.net/api/billing/stripe/webhook
```

订阅以下事件：

```text
checkout.session.completed
customer.subscription.created
customer.subscription.updated
customer.subscription.deleted
invoice.paid
invoice.payment_failed
charge.refunded
```

复制 endpoint signing secret 并配置：

```text
STRIPE_WEBHOOK_SECRET=whsec_xxx
```

使用 Stripe 的 **Send test webhook** 验证事件返回 HTTP `2xx`，并确认没有签名验证错误。

## 验收流程

### 月付订阅

1. 在应用 `/prices` 页面登录测试账户并选择 Pro 月付。
2. 使用测试卡 `4242 4242 4242 4242` 完成支付。
3. 在 Stripe 中确认 Customer、Subscription 为 `active`，周期为 month。
4. 确认 `checkout.session.completed`、`customer.subscription.created`、`invoice.paid` 已送达。
5. 在应用中确认套餐为 Pro、当月额度为 20GB。

### 年付订阅

1. 选择 Pro 年付并完成测试支付。
2. 确认 Stripe 周期为 year。
3. 确认应用按自然月发放 20GB，而不是一次性发放 240GB。
4. 执行或等待年付配额任务，确认同一自然月不会重复发放。

### 退款

退款接口：

```http
POST /api/auth/subscriptions/refund
```

自动退款必须满足：首次订阅、订阅不超过 7 天、用量低于包含配额的 5%，并已完成 MFA。

在 Stripe 中确认：

- PaymentIntent 下出现 Refund；
- Subscription 状态变为 `canceled`；
- Events 中出现退款和订阅取消事件。

应用中确认返回 `refunded: true`，本地订阅降级为 `FREE`，重复请求不会产生第二笔退款。

### 取消、到期和失败支付

使用 `customer.subscription.deleted` 验证订阅结束后的降级路径：

- 套餐变为 `FREE`；
- Free 限量配额重新生效（当前目录为 5GB/月）；
- Pro 权益被移除；
- 后续超额按 Free 策略处理。

使用测试失败卡 `4000 0000 0000 0341` 验证 `invoice.payment_failed`：账户进入欠费/宽限状态，不能错误发放配额；恢复支付后状态应恢复正常。

## 边界条件

| 场景 | 预期结果 |
| --- | --- |
| 超过 7 天退款 | 拒绝退款 |
| 用量达到或超过 5% | 拒绝退款 |
| 非首次订阅退款 | 拒绝退款 |
| 未完成 MFA | 拒绝退款 |
| 重复退款 | 幂等拒绝，不重复扣款 |
| PaymentIntent 不存在 | 返回支付记录错误 |
| 订阅到期/取消 | 自动降级为限量 Free |

## 发布后检查

记录以下信息作为 UAT 验收凭证：

- Stripe Test Mode Price ID；
- Webhook endpoint 与最近事件；
- Checkout、Subscription、Refund 对象 ID；
- UAT workflow run URL；
- Accounts 和 Portal 健康检查结果；
- 运行镜像对应的完整 Git SHA。
