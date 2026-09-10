# Gmail SMTP Setup Guide for Cloud Run

本文档详细说明如何配置 accounts.svc.plus 服务以使用 Gmail SMTP 发送邮件，并部署到 Google Cloud Run。

## 1. 获取 Gmail 应用专用密码 (App Password)

由于安全原因，您**不能**直接使用 Gmail 的登录密码。必须生成一个“应用专用密码”。

1.  登录您的 Google 账号。
2.  前往 **安全性 (Security)** 设置页面：[https://myaccount.google.com/security](https://myaccount.google.com/security)
3.  确保您已开启 **两步验证 (2-Step Verification)**（如果未开启，必须先开启）。
4.  在“两步验证”设置下方（或搜索栏搜索），找到 **应用专用密码 (App passwords)**。
    > **注意**：如果找不到此选项，可能是因为两步验证未开启。
5.  创建一个新密码：
    *   **应用 (App)** 选择 "Mail" (邮件)。
    *   **设备 (Device)** 选择 "Other" (其他)，填入名称如 `svc-plus-smtp`。
6.  点击 **生成 (Generate)**。
7.  复制生成的 **16位字符密码**（去掉空格）。这将是您的 `SMTP_PASSWORD`。

---

## 2. 配置 Google Cloud Secret Manager

为了安全地在 Cloud Run 中使用凭据，我们需要创建两个独立的 Secret：`smtp-username` 和 `smtp-password`。

请在您的 GCP 终端或者 Cloud Shell 中运行以下命令（请替换为您的真实信息）：

### 2.1 创建 SMTP 用户名 Secret
该 Secret 存储用于 SMTP 认证的邮箱地址。当前线上取值为 `no-reply@xworktech.com`。

```bash
gcloud secrets create smtp-username --replication-policy="automatic"

# 已存在时直接加新版本即可
printf "no-reply@xworktech.com" | gcloud secrets versions add smtp-username --data-file=-
```

### 2.2 创建 SMTP 密码 Secret
该 Secret 存储步骤 1 中生成的 16 位应用专用密码。

```bash
# 替换 xxxx xxxx xxxx xxxx 为您的 16 位 Google 应用密码
gcloud secrets create smtp-password --replication-policy="automatic"

# 添加版本
printf "xxxx xxxx xxxx xxxx" | gcloud secrets versions add smtp-password --data-file=-
```

---

## 3. 绑定 svc.plus 域名

如果您希望发件人显示为 `@svc.plus` 后缀（如 `admin@svc.plus`），有两种情况：

### 情况 A：使用 Google Workspace (企业邮箱) —— 当前采用
*   **适用场景**：域名托管在 Google Workspace，用该租户下的真实用户做 SMTP 认证。
*   **当前线上配置**：
    *   **SMTP Host**: `smtp.gmail.com`
    *   **SMTP Port**: `587`（STARTTLS）
    *   **SMTP Username**: `no-reply@xworktech.com`
    *   **SMTP From**: `XWorkmate <no-reply@xworktech.com>`
    *   **SMTP Password**: 该账号的**应用专用密码**（16 位，去掉空格）。
*   **envelope sender 必须等于认证账号**，否则 Gmail 返回
    `550-5.7.1 ... not allowed to send as this address`。要用别的地址发信，
    必须先在 Workspace 里把它配成该账号已验证的 "send mail as" 别名。
*   **想改用 `no-reply@svc.plus`**：把 svc.plus 作为**域名别名 (Domain alias)**
    加进同一个 Workspace 租户即可 —— 免费、不额外占席位，`no-reply@xworktech.com`
    会自动获得 `no-reply@svc.plus`。不要用**辅助域 (Secondary domain)**，那会把
    `no-reply@svc.plus` 变成独立用户，多吃一个付费席位。
    切过去时只需改 `SMTP_FROM` 一行；`SMTP_USERNAME` 保持认证账号不变。

### 情况 B：使用个人 Gmail 代发
*   **适用场景**：您持有普通 Gmail 账号 (例如 `shenlan@gmail.com`)，但拥有 `svc.plus` 域名。
*   **配置方式**：
    1.  在 Gmail 网页端设置 -> 账号和导入 -> "发送邮件为" 中，添加另一个电子邮件地址。
    2.  输入您的域名邮箱（如 `no-reply@svc.plus`）。
    3.  配置 SMTP 服务器（通常使用 Gmail 的 SMTP）。
    4.  验证域名所有权（输入发送到域名邮箱的验证码）。
*   **风险**：接收方可能会看到 "由 gmail.com 代发" 的提示。

### 情况 C：直接使用个人 Gmail (无域名)
*   **适用场景**：您只想用个人的 `gmail.com` 邮箱发送通知，不强制要求域名。
*   **配置方式**：
    *   **SMTP Username**: 您的完整 Gmail 地址 (例如 `yourname@gmail.com`)。
    *   **SMTP Password**: 您的 16 位**应用专用密码**。
    *   **SMTP From**: 设置为 `XWorkmate <yourname@gmail.com>`。
    *   **Cloud Run Env**: 将 `SMTP_FROM` 环境变量修改为您的 Gmail 地址。

---

## 4. 部署到 Cloud Run

确保您的 `deploy/gcp/cloud-run/service.yaml` 已包含以下配置（已在代码库中更新）：

```yaml
        # --- SMTP Configuration ---
        - name: SMTP_HOST
          value: "smtp.gmail.com"
        - name: SMTP_PORT
          value: "587"
        - name: SMTP_FROM
          value: "XWorkmate <no-reply@xworktech.com>"
        - name: SMTP_USERNAME
          valueFrom:
            secretKeyRef:
              name: smtp-username
              key: latest
        - name: SMTP_PASSWORD
          valueFrom:
            secretKeyRef:
              name: smtp-password
              key: latest
```

执行部署命令（`CLOUD_RUN_SERVICE_YAML` 默认解析为 `deploy/gcp/cloud-run/$(CLOUD_RUN_ENV)-service.yaml`）：

```bash
GCP_PROJECT=<project> CLOUD_RUN_ENV=prod make cloudrun-deploy
```

部署完成后，Cloud Run 实例将自动读取 Secrets 作为环境变量，服务即可使用 Gmail SMTP 发送验证邮件。

---

## 5. 域名的邮件 DNS 由 GitOps 管理，不要手工点

SPF / DMARC / MX 的期望状态声明在 `x-evor/gitops` 的
`resources/xworktech.com/prod/cloudflare/email-dns.yaml`，由
`ai-workspace-infra/playbooks` 的 `configure_resend_dns.yml` +
`.github/workflows/configure-resend-dns.yml` 对账，凭据取自 Vault
`kv/data/prod/xworktech-email`（`cloudflare_api_token`）。新增或修改记录
应改那个 YAML 后跑 workflow，而不是在 Cloudflare 控制台手改。

例外：Google Workspace 的 DKIM 密钥对必须在 Admin 控制台
（Apps → Google Workspace → Gmail → Authenticate email）生成——DNS 侧造不出来。
生成后把公钥值写回上述 GitOps YAML，再由 workflow 发布 `google._domainkey` TXT。
