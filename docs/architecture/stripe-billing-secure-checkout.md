# Stripe Billing 安全结算架构

## 目标

将 Stripe 支付收敛为一条可审计、可幂等的服务端链路：Portal 不接触 Stripe
Secret Key；Accounts 是唯一的 Stripe 服务端集成边界；Webhook 是本地订阅和
权益状态的最终来源。

产品状态流转为：

```text
Free（限量） → Pro 月付/年付 → 退款、取消或到期 → Free（限量）
```

团队定制服务不通过自助 Stripe Checkout 创建，由商务/运营流程单独开通。

## 完整链路

```text
Portal
  ↓ planId + public stripePriceId
Accounts
  ↓ Vault 注入 STRIPE_SECRET_KEY
Stripe Checkout Session
  ↓ Webhook
Accounts
  ↓ 签名验证、事件去重
订阅 / 配额 / 退款 / Free 降级
```

## 组件职责

| 组件 | 职责 | 禁止事项 |
| --- | --- | --- |
| Portal | 从 Accounts 套餐目录展示可售计划；携带登录会话提交受控 checkout payload | 保存或使用 `sk_*`、`whsec_*`；直接调用 Stripe Secret API |
| Accounts | 校验套餐目录；创建 Checkout Session；创建 Customer Portal Session；处理退款、取消、权益和配额 | 信任浏览器提交的金额、套餐类型或任意 Stripe 参数 |
| Vault | 按环境向 Accounts 注入 Secret Key 和 Webhook Secret | 向 Portal 注入 Stripe Secret |
| Stripe | 托管支付方式、Checkout、订阅、发票和事件投递 | 将前端成功跳转作为权益已生效的唯一凭据 |

## Checkout 边界

Portal 只允许提交以下受控字段：

```text
planId
stripePriceId
mode
productSlug
sourcePath
```

Accounts 读取 `billing_plans`，同时校验：

1. `planId` 对应活动套餐；
2. 套餐中的 `stripe_price_id` 与请求一致；
3. `subscription` 套餐只能用 subscription mode；
4. `paygo_topup` 套餐只能用 payment mode；
5. 目录已有付费 Price 后，拒绝环境变量白名单以外的回退路径。

Accounts 用服务端保存的 `STRIPE_SECRET_KEY` 创建 Checkout Session，并写入
`user_id`、`plan_id`、`product_slug`、`kind` 等服务端元数据。Portal 收到的只有
Stripe 返回的 Checkout URL。

## Vault 与环境隔离

UAT：

```text
kv/uat/billing-service/SANDBOX_STRIPE_SECRET_KEY
kv/uat/billing-service/SANDBOX_STRIPE_WEBHOOK_SECRET
```

生产：

```text
kv/prod/billing-service/PROD_STRIPE_SECRET_KEY
kv/prod/billing-service/PROD_STRIPE_WEBHOOK_SECRET
```

部署流程根据 `STRIPE_MODE` 将同一环境中的一对密钥映射为 Accounts 运行时的：

```text
STRIPE_SECRET_KEY
STRIPE_WEBHOOK_SECRET
```

测试和生产不可共享 `sk_*` 或 `whsec_*`。Price ID 是公开的商业配置，存放在
Accounts 套餐目录而非 Vault。

## Webhook 处理

Stripe 将事件投递到：

```text
POST /api/billing/stripe/webhook
```

Accounts 使用 `STRIPE_WEBHOOK_SECRET` 验证签名和时间戳；事件先写入
`stripe_webhook_events` 进行审计与去重，再执行业务更新。重复事件必须返回成功
且不重复发放配额、余额或退款。

| Stripe 事件 | Accounts 行为 |
| --- | --- |
| `checkout.session.completed` | 记录结算上下文；PAYG 仅在服务端 `kind=paygo` 且已付款时计入余额 |
| `customer.subscription.created/updated` | 同步订阅和套餐权益 |
| `invoice.paid` | 重置当前计费周期配额并清理欠费状态 |
| `invoice.payment_failed` | 标记欠费，等待后续宽限/停用策略处理 |
| `customer.subscription.deleted` | 降级为限量 Free 并重置 Free 周期配额 |
| `charge.refunded` | 用于退款审计；退款请求由 Accounts 以幂等键执行 |

## 退款与降级

自动退款仅在以下条件同时满足时执行：首次订阅、创建时间不超过 7 天、用量低于
包含配额的 5%、用户已完成 MFA、且存在可退款 PaymentIntent。

退款按稳定 Idempotency Key 调用 Stripe Refund API，然后取消 Stripe 订阅，更新本地
订阅状态并执行 `downgradeToFreePlan`。无论退款、取消还是订阅到期，最终都回到
限量 Free，而不是保留 Pro 权益。

## 验收与可观测性

UAT 必须完成以下证据闭环：

1. Stripe Test Mode 中创建月付与年付订阅；
2. Webhook Delivery 返回 HTTP `2xx`，Accounts 未出现签名错误；
3. 月付发放当月配额，年付按自然月发放而非一次性全年配额；
4. 满足条件的退款只产生一笔 Stripe Refund 并降级为 Free；
5. 重放 Webhook 或重复退款请求不产生重复副作用；
6. 记录 Stripe 对象 ID、Webhook Event ID、UAT workflow run 和运行镜像完整 SHA。

