# 启用 XWorkmate 事务邮件通道（Google Workspace SMTP）

**类型**: 部署相关
**严重级别**: P1 (Critical) — 未完成时用户无法完成注册
**最后更新**: 2026-09-10
**负责人**: SRE Team

---

## 📋 问题描述

`console.svc.plus/register` 提交邮箱后提示发送验证码失败。

线上 `accounts.svc.plus` 由 Cloud Run 服务 `accounts-svc-plus` 提供
（`gitops/topology/prod/serverless/runtime-topology.yaml` 中 prod 权重为
`selfhost: 0 / serverless: 100`）。原 SMTP 出口为 `smtp.qq.com`，认证账号
`manbuzhe2009@qq.com`。Cloud Run 没有 VPC connector 也没有 Cloud NAT，出口是
漂移的 GCP 地址段，QQ 的反滥用策略会拒绝，发信在 10s 超时内失败。

代码已切换到 Google Workspace SMTP（accounts#140），但**凭据与域名 DNS 属于
运行时配置，不随代码合并生效**。本文档就是把剩余的人工步骤跑完。

### 一个容易误判的地方

`cmd/accountsvc/main.go` 在 SMTP 的 host / username / password / from 任一为空时，
会把 `emailVerificationEnabled` 置为 false，此时 `/auth/register/send`
**直接返回 200「verification email sent」而不发信**。

所以：

- 页面报错 → 凭据是配上的，是投递真的失败
- 页面成功但收不到信 → 凭据缺失被静默降级，或 DNS 认证未通过被收件方丢弃

两者的排查方向完全相反，先分清再动手。

---

## 🎯 影响范围

- **服务**: `accounts-svc-plus`（区域 `asia-northeast1`；项目按环境从 Vault 解析，见下）
- **影响功能**: 新用户注册验证码、账号邮箱验证、重置密码/找回
- **影响用户**: 全部新注册用户；已注册用户的密码找回
- **未来影响**: billing-service 的催缴邮件规划复用同一发信身份

---

## 🔍 诊断步骤

### 0. 先解析目标项目

项目号不要写死在文档或命令里——它随环境和迁移变化，而真源是 Vault。本页所有
`gcloud` 命令都假设你已经导出了它：

```bash
export VAULT_ADDR=https://vault.svc.plus
```

```bash
export GCP_PROJECT="$(vault kv get -field=GCP_PROJECT_ID kv/prod/serverless/gcp)"; echo "project=$GCP_PROJECT"
```

同一套 Vault 路径也是部署流水线读取项目的地方，所以这里解析出来的一定和线上一致。
注意 uat 与 prod 目前解析到**同一个项目**，靠服务名区分环境——所以在其中一个环境上
看到的 Secret Manager 状态，对另一个同样成立。

### 1. 确认走的是哪条部署链路

```bash
grep -A3 "weight:" gitops/topology/prod/serverless/runtime-topology.yaml
```

`serverless: 100` 表示线上是 Cloud Run，改 ansible/VPS 那条线不会生效。

### 2. 确认容器实际拿到的 SMTP 配置

```bash
gcloud run services describe accounts-svc-plus --region asia-northeast1 --project "$GCP_PROJECT" --format=yaml | grep -A2 SMTP_
```

### 3. 确认域名邮件 DNS 是否就位

```bash
for d in xworktech.com svc.plus; do echo "--- $d"; dig +short MX $d; dig +short TXT $d | grep -i spf; dig +short TXT google._domainkey.$d | head -c 80; echo; dig +short TXT _dmarc.$d; done
```

四类记录缺任何一类，邮件都可能发得出去却被收件方判为垃圾。

### 4. 看真实的 SMTP 错误

```bash
gcloud logging read 'resource.labels.service_name="accounts-svc-plus" AND jsonPayload.msg=~"verification"' --project "$GCP_PROJECT" --limit 20 --freshness 1h
```

---

## 🔧 修复方案

**五个步骤的顺序不能调换。** 第 3 步（DNS）必须先于第 5 步（凭据生效）：
DNS 没就位就切 Gmail，邮件发得出去但会进垃圾箱——那比现在的显式报错更难查，
因为服务端一切正常，只有收件人那侧安静地收不到。

### 步骤 1：Google Admin 控制台

需要 Workspace 超级管理员权限。

1. **建发信账号** `no-reply@xworktech.com`（占 1 个席位）
2. **生成应用专用密码**：该账号需先开启两步验证，然后
   `安全性 → 应用专用密码`，应用选 Mail，设备选其他，命名如 `accounts-svc-plus`。
   记下 16 位密码（去掉空格），后面第 4 步用
3. **把 svc.plus 加为域名别名 (Domain alias)**：
   `账号 → 域名 → 管理域名 → 添加域名 → 用户别名域`

   > ⚠️ 不要选**辅助域 (Secondary domain)**。域名别名免费且不占席位，
   > `no-reply@xworktech.com` 自动获得 `no-reply@svc.plus`，是同一个邮箱；
   > 辅助域会把 `no-reply@svc.plus` 变成独立用户，多吃一个付费席位。

4. **给 svc.plus 单独生成 DKIM**：
   `应用 → Google Workspace → Gmail → 用户身份验证邮件`，选中 svc.plus
   生成新密钥

   > 域名别名**不继承**父域的 DKIM 密钥，xworktech.com 那把在 svc.plus 上无效。

### 步骤 2：把 DKIM 公钥填进 GitOps

仓库 `ai-workspace-infra/gitops`：

```yaml
# resources/svc.plus/prod/cloudflare/email-dns.yaml
    dkim:
      - name: google._domainkey.svc.plus
        type: TXT
        selector: google
        ttl: 3600
        content: "v=DKIM1; k=rsa; p=<粘贴 Admin 控制台生成的公钥>"
```

`content` 为空时对账器会跳过该记录并打印提示，不会把坏 selector 推进 DNS。

### 步骤 3：跑 DNS 对账 workflow

仓库 `ai-workspace-infra/platform-ops-toolkit` → Actions → **Configure Email DNS**
→ Run workflow，`vault_env_path: prod`。

> ⚠️ **Use workflow from 必须选一个 `v*` tag，不能选 main。** Vault role 的 `ref`
> 绑定限定了 `refs/tags/v*` 与 `refs/heads/release/v*`；从 main 触发会在 Vault 步骤
> 认证失败，而报错不会提示分支选错了。选包含该 workflow 的最新 tag。

zone matrix 一次跑完 `xworktech.com` 和 `svc.plus`，串行执行（两份 policy 编辑
同一个 Cloudflare 账号，且各自的 apex SPF 是整条改写而非差异更新）。Cloudflare
凭据取自 Vault `kv/data/<env>/serverless/cloudflare` 的 `CLOUDFLARE_API_TOKEN`
——与 serverless-orchestrator 共用同一条，不另建 mail 专用副本。

期望结果：两个域各就位 5 条 Google MX、1 条 apex SPF、1 条 DKIM、1 条 DMARC。
同名多条记录会被 delete-then-create 收敛，所以手工误加的重复条目也会在这一步被清掉。

### 步骤 4：Vault 种入 SMTP 凭据

三个环境共用同一个发信身份，各自路径下填相同的值：

```bash
export VAULT_ADDR=https://vault.svc.plus
for env in sit uat prod; do
  vault kv put "kv/${env}/platform/smtp/google" \
    username="no-reply@xworktech.com" \
    password="<16 位应用专用密码>"
done
```

路径用 `platform` 而非 `accounts` 作用域：这个邮箱身份不属于 accounts 服务，
billing-service 的催缴邮件规划用的是同一个身份。按服务分路径意味着同一个密码
存两份、轮换两次，最终变成只轮换了一次。

> 路径按环境切是为了不动 Vault policy——各环境 role 现有的 `kv/data/<env>/*`
> 授权直接覆盖。代价是轮换要改三处。若改为单一轮换点
> `kv/data/platform/smtp/google`，需给三个 env role 各加一条读授权
> （`kv/data/CICD` 是这种「三环境共读」的先例）。

### 步骤 5：部署生效

跑一次 `platform-ops-toolkit` 的 **serverless-orchestrator**
（operation: `deploy`，对应环境的 `vault_env_path`）。

`cloud_run` job 会在部署前执行
`.github/scripts/serverless/sync_smtp_secrets.sh`：从 Vault 读取凭据，写入
GCP Secret Manager 的 `smtp-username` / `smtp-password`，然后新 revision 通过
`secretKeyRef` 读到。

**绑定是有条件的。** `deploy_cloudrun_services.sh` 只在 `smtp-username` 与
`smtp-password` 两个 secret 都能 describe 到时才加 `--set-secrets`；否则打印
`SMTP secrets not present in Secret Manager; skipping secret bindings.` 并继续部署。
结果是容器里两个变量未设置，accounts 关闭邮件发送、注册接口静默返回 200。

部署日志里必须看到 `Binding Secret Manager SMTP credentials` 这一行，否则这次发布
没有邮件能力——而每一步都会显示成功。

同步步骤的三个行为值得知道：

| 情况 | 行为 |
|---|---|
| Vault 路径不存在 | `::notice::` 跳过，不中断部署（该环境保持不发信） |
| 路径存在但缺 key | **失败** —— 这是配错了，不是刻意不配 |
| 值与 Secret Manager 现值相同 | 不写新版本（避免版本数随部署次数膨胀、污染轮换审计） |

---

## ✅ 验证方法

### 1. DNS 四类记录齐全

```bash
for d in xworktech.com svc.plus; do echo "--- $d"; dig +short MX $d | head -1; dig +short TXT $d | grep -i spf; dig +short TXT google._domainkey.$d | head -c 40; echo; dig +short TXT _dmarc.$d; done
```

### 2. 容器拿到了正确配置

```bash
gcloud run services describe accounts-svc-plus --region asia-northeast1 --project "$GCP_PROJECT" --format=yaml | grep -A2 SMTP_HOST
```

应为 `smtp.gmail.com`。

### 3. 端到端发信

在 `console.svc.plus/register` 用一个真实可收件的外部邮箱（建议用 Gmail，
便于看认证结果）走一次注册。

收到后在原始邮件头里确认：

```
Authentication-Results: ... spf=pass ... dkim=pass ... dmarc=pass
```

三项全 pass 才算真正完成。只要有一项 fail，邮件迟早会进垃圾箱。

### 4. 发件人显示正确

发件人应显示为 `XWorkmate <no-reply@xworktech.com>`，正文页眉为
`XWorkmate` / `XConnect · AI Workspace`，页脚含 `XWork Technologies` 与
`Powered by the svc.plus platform`。

preview 环境显示名为 `XWorkmate (Preview)` —— 非生产与生产共用同一发信身份，
显示名是收件人区分测试邮件的唯一线索。

---

## 🔙 回滚计划

**DNS 层**：`Configure Email DNS` 是声明式对账。回滚就是把 GitOps 里的
`email-dns.yaml` 改回上一版再跑一次 workflow，不要在 Cloudflare 控制台手改
——手改的记录会在下次对账时被覆盖，且不留痕迹。

**凭据层**：Secret Manager 保留历史版本，回滚到上一版本：

```bash
gcloud secrets versions list smtp-password --project "$GCP_PROJECT"
gcloud secrets versions access <上一个版本号> --secret smtp-password --project "$GCP_PROJECT"
```

注意：下次部署时同步步骤会再次把 Vault 的值推上去。真正的回滚要改 Vault，
Secret Manager 只是投递形式。

**服务层**：Cloud Run 保留旧 revision，紧急时切流量：

```bash
gcloud run services update-traffic accounts-svc-plus --region asia-northeast1 --project "$GCP_PROJECT" --to-revisions=<上一个 revision>=100
```

**最保守的降级**：把 Vault 路径下的 `username` / `password` 清空并重新部署，
accounts 会关闭邮件发送。注册接口返回 200 但不发信，验证码可从 Cloud Run
日志中取得（`issued registration verification code`）。这条路能让注册流程在
邮件通道不可用时仍可人工完成，代价是验证码出现在日志里——**只应作为临时手段**。

---

## 📎 相关资料

| 内容 | 位置 |
|---|---|
| 架构与设计取舍 | `docs/architecture/transactional-email.md` |
| SMTP 配置细则与发件身份说明 | `docs/SMTP_GMAIL_SETUP.md` |
| DNS 声明（xworktech.com / svc.plus） | `ai-workspace-infra/gitops` → `resources/<domain>/prod/cloudflare/email-dns.yaml` |
| DNS 对账 playbook | `ai-workspace-infra/playbooks` → `configure_email_dns.yml` |
| DNS 对账 workflow（调度入口） | `ai-workspace-infra/platform-ops-toolkit` → `.github/workflows/configure-email-dns.yaml` |
| Vault → Secret Manager 同步脚本 | `ai-workspace-infra/platform-ops-toolkit` → `.github/scripts/serverless/sync_smtp_secrets.sh` |
| 邮件模板与文案 | `api/email_template.go` |
| SMTP 客户端实现 | `internal/mailer/mailer.go` |
