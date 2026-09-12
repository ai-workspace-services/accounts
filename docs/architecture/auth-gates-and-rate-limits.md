# 登录闸门与限流

## 问题的形状

2026-09-11 的事故本身只是 `register` 里漏了一行 `Active: true`。值得写下来的不是那一行，
而是它为什么要花一整天才被定位——以及为什么同一个形状在这个服务里出现了五次。

五处发现是同一件事：**一个本该显式写出的状态，被默默地推断了。**

- 注册漏写 `Active` —— Go 的零值替它做了决定
- 闸门条件挂载 —— 配置缺失替它做了决定
- 三种"停用"共用一个错误码 —— 读日志的人只能靠字节数去猜
- 权限矩阵未登记的项 —— map 的缺省返回替它做了决定
- 测试替身自作主张补上 `true` —— 测试替真实存储做了决定

这些都不是"写错了"，而是"没写"。Go 的零值、Gin 的可选中间件、map 的缺省返回，
三者都极其乐于把空缺填成一个看起来能跑的值。

## 三道闸门

账号"能不能用"由三个互不相干的状态决定，分属两张表：

```text
users.active                         → RequireActiveUser  → 除 login 外的一切
account_quota_state.suspend_state    → RequireActiveUser  → 除 login 外的一切
                                       rejectIfBillingSuspended → login 也查
account_quota_state.proxy_access_state → pause / resume    → 只管 VLESS
```

三者没有共享的判定入口，但**前两者返回完全相同的响应**：

```json
{"error":"account_suspended","message":"your account has been suspended"}
```

**这正是它最危险的性质。**事故当天无法从日志区分"账号未激活"和"欠费挂起"，
最后是靠响应体字节数反推出来的：

```text
401 已知体 94 B  → 日志 responseSize 196  ⇒ 头部开销 102 B
403 日志 responseSize 175              ⇒ 体 73 B
{"error":"account_suspended",...} 恰好 73 B
```

能这样反推纯属运气——两个分支的文案恰好不等长就无从下手了。

第三道 `pause` / `resume` 看起来像是"停用/恢复账号"，实际只动 VLESS，
返回 `"vless access resumed"`。事故中我据此向运维建议用 `resume` 恢复被锁账号：
它会返回 200，而用户侧毫无变化。

## 登录与其后的不对称

`login` 不读 `users.active`，`RequireActiveUser` 读。于是一个未激活账号的体验是：

```text
POST /api/auth/login     200   Set-Cookie: xc_session=…
GET  /api/auth/session    403   account_suspended
GET  /api/auth/session    403
GET  /api/auth/session    403
```

登录明确成功、Cookie 正常下发、控制台开始渲染，然后每一个后续请求被拒。
对用户是"登录成功又被踢出去"，对排障是"200 后面跟着一串 403"——
两边都拿不到有用信息。这是这个 bug 能潜伏这么久的直接原因。

## 闸门是条件挂载的

```go
if h.tokenService != nil {
        authProtected.Use(h.tokenService.AuthMiddleware())
        authProtected.Use(auth.RequireActiveUser(h.store))
}
```

token service 没配好的部署，整条受保护路由会以**无闸门状态**对外服务，且不报任何错。
方向是 fail-open。

这不是假设风险：`admin_user_active_test.go` 的第一版就是在这样一个 router 上"通过"的，
断言全绿，证明了零。测试里配上 token service 之后才开始真正失败。

## 测试替身与真实存储的分歧

```go
// memoryStore.CreateUser
stored.Active = true          // 无条件

// postgresStore.CreateUser
args = append(args, user.Active)   // 调用方给什么写什么
```

`users.active` 的 schema 默认是 `TRUE`，但 postgres 实现总是显式写这一列，
默认值永远不生效。于是"注册漏写 Active"在单测里永远绿灯——它正是本次事故的根因。

## 限流的现状与边界

`register` / `register/send` / `login` 现在按 **IP + 邮箱/标识符**两个维度独立限流，
`register` 与 `register/send` 共用同一份按邮箱的额度（两者回答同一个"邮箱是否已注册"的
问题，换端点不应换来新额度）。命中一律返回通用 `429 rate_limited`，不透露是哪个维度触发，
避免它本身成为新的侧信道。

限流状态是**进程内**的，Cloud Run 扩容或重新部署即清零。它把枚举成本从"秒"抬到"小时"，
但不是横向的硬保证。当前流量下可以接受，不要当成已解决。

仍未收口的面：

- `register/verify` 的验证码是 6 位十进制（10⁶）、TTL 10 分钟，**既无尝试次数限制、
  也不在限流内**。同一服务里的 MFA 有 `maxMFAVerificationAttempts = 5` 加 5 分钟锁定，
  邮箱验证什么都没有。这是三者里唯一可直接利用的缺口。
- `GET /api/auth/mfa/status?identifier=…` 不需要鉴权也不限流。事故当天正是靠它从日志
  还原出"谁在登录"，攻击者同样可以。
- 密码登录没有账号级失败锁定，只有速率限制兜着；MFA 有锁定。

## 修复方式

以下都是断裂式改法。消费方只有自家 portal 与 console，同一次发布内一起改完，
**不保留兼容别名、不加开关、不设过渡期**——多留一个旧值，就是多留一份"两种含义共用一个
符号"的债，而那正是本次事故的成本来源。

### 1. 拆开错误码

```text
account_inactive     users.active = false
billing_suspended    quota.suspend_state = suspended
proxy_paused         quota.proxy_access_state = paused
```

`account_suspended` 直接删除，不做别名。前端错误映射同批更新。

### 2. login 显式判定 Active

`login` 读 `users.active`，未激活直接返回 `403 account_inactive`，不再下发 session。
这会改变"未激活账号也能拿到 200"的现有行为——**这正是要改掉的东西**，
而不是需要保留的契约。

### 3. 闸门 fail-closed

`tokenService` 缺失时启动失败，不再静默跳过中间件。没有任何合理部署需要一个不鉴权的
受保护路由；能跑起来本身就是 bug。

### 4. 删掉 memory store 的 `Active = true`

让两套实现在这个字段上行为一致。这会打破若干依赖"建完即 active"的既有测试——
让它们显式写 `Active: true`，这正是被测代码本来就该做的事。

### 5. 验证码对齐 MFA 标准

给 `verifyEmail` 加尝试计数与锁定，并纳入限流。6 位十进制的熵在有锁定的前提下可以接受；
没有锁定则必须提高熵。二选一，不要两者都不做。

### 6. 补齐剩余限流面

`mfa/status` 纳入同一套按 IP / 标识符的预算；密码登录补账号级失败计数，与 MFA 对齐。

## 发布顺序与老用户保障

上面的改法本身是断裂式的，但**发布顺序不能断裂**。接口契约可以一次改干净，
用户的登录能力不能中断一秒。下面的顺序不可调换。

### 第 0 步：先修数据，这一步必须在任何闸门收紧之前

`login` 一旦开始判定 Active，现存 `active=false` 的账号会从"能登录但什么都做不了"
变成"完全登不进去"，而且没有任何自助路径。所以数据修复必须先行。

这次修复可以被论证为安全，而不是靠猜。全库只有一个地方会主动把**用户**置为 false：

```go
// cmd/accountsvc/main.go —— 应用商店审核账号，配置关闭时停用
if !cfg.Enabled {
        if reviewUser != nil && reviewUser.Active {
                reviewUser.Active = false
```

（`ensureDefaultBillingPlans` 里的 `legacyTrial.Active = false` 是 `BillingPlan.Active`，
与用户无关。）

因此：**除审核账号外，任何 `active=false` 的用户行都只可能来自注册时的零值**。
先核对再修改：

```sql
-- 先看清楚要动哪些行，确认没有意料之外的账号
SELECT uuid, email, username, active, created_at
FROM users
WHERE active = false
ORDER BY created_at;

-- 排除审核账号后修复（<review-email> 取自部署配置）
UPDATE users
SET active = true
WHERE active = false
  AND lower(email) <> lower('<review-email>');
```

> **这条 SQL 只在此刻有效。**一旦 activate / deactivate 管理端点上线，
> `active=false` 就成了管理员可以合法设置的状态，无差别刷 true 会撤销真实的封禁。
> 这是一次性的历史数据订正，不是可复用的运维手段。

### 第 1 步：错误码拆分 + 前端映射，同批发布

这一步不影响任何人能否登录，只影响提示文案。server 与 portal / console 必须同一批次
发布：先发 server 会让前端落到通用兜底文案，先发前端则新码还不存在。两边都不会锁人，
但没有理由让任何一侧单独先走。

### 第 2 步：login 判定 Active

只有在第 0 步确认 `active=false` 已清零（审核账号除外）之后才能发。发之前重跑一次
上面的 SELECT，返回应当只剩审核账号。

### 第 3 步：闸门 fail-closed

改成启动失败之前，先确认**每个环境**都已配置 `tokenService`，否则这次发布会把一个
本来在跑的部署直接变成起不来。这是发布前的预检，不是给代码加兼容开关——
确认过了就直接改成硬失败。

```bash
# 每个环境各跑一次：受保护端点在无凭据时必须是 401，不能是 200
curl -s -o /dev/null -w '%{http_code}\n' https://<host>/api/auth/session
```

返回 200 说明该环境当前就在无闸门状态运行，必须先补配置再发。

### 第 4 步：验证码锁定

锁定会带来"正常用户手滑被挡"的新风险。两条约束把它压住：计数按每次验证码签发重置
（用户点"重新发送"即获得新预算），锁定时长与 MFA 对齐为 5 分钟而非小时级。

### 对已登录用户的影响

以上改动都不触碰 session 存储，已签发的 session 不会失效，不存在"全员被强制登出"
这一类风险。唯一会改变既有行为的是第 2 步，而它被第 0 步挡在安全侧。

## 一条约定

- **决定"能不能用"的字段，构造时必须显式赋值，禁止依赖零值。**
- **闸门缺失依赖时启动失败，而不是跳过。**

这两条如果当初就在，本文记录的问题里至少四个不会存在。

## 相关

- `docs/architecture/design-decisions.md` —— root 账号的唯一性约束（`enforceRootProfile`
  每次启动强制 root 为 active，因此 `deactivate` 对 root 明确拒绝而非假装成功）
- `internal/auth/ratelimit.go` —— 限流实现与其进程内边界
- `api/admin_user_active_test.go` —— `resume` 不恢复 `users.active` 的回归断言
