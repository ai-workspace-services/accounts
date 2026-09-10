# 事务邮件投递架构

## 目标

把注册验证码、邮箱验证、密码找回三封事务邮件收敛为一条可审计的链路：凭据只有
Vault 一个真源；发件身份由 DNS 显式授权；投递失败在服务端可见，而不是安静地
消失在收件人的垃圾箱里。

## 两个平面

事务邮件依赖两条互相独立、但必须同时成立的链路。

```text
凭据平面 —— 把发信身份送进容器
  Vault → Secret Manager → Cloud Run → smtp.gmail.com

                    ⟍                    ⟋
                     no-reply@xworktech.com
                    ⟋                    ⟍

DNS 平面 —— 让收件方相信这个身份
  GitOps → Toolkit → Playbooks → Cloudflare
```

两者没有共享的运行时依赖，可以独立部署——**这正是它最危险的性质**。凭据先行会让
邮件在没有 SPF/DKIM 背书的情况下发出：服务端一切正常，日志全绿，只有收件人那侧
安静地收不到。排查这种故障远比排查一个显式的 SMTP 报错困难。

**因此：DNS 必须先于凭据生效。**

## 凭据平面

```text
Vault   kv/data/<env>/platform/smtp/google
  ↓ toolkit/.github/scripts/serverless/sync_smtp_secrets.sh
GCP Secret Manager   smtp-username / smtp-password
  ↓ Cloud Run secretKeyRef
容器环境变量
  ↓ entrypoint.sh envsubst → config/account.cloudrun.yaml
internal/mailer → smtp.gmail.com:587 (STARTTLS)
```

### 为什么中转一次

Cloud Run 只能通过 `secretKeyRef` 读取机密，读不了 Vault；而机密的源头必须是
Vault。所以 Secret Manager 是投递形式，不是真源——**回滚要改 Vault，改 Secret
Manager 会被下次部署覆盖**。

同步步骤只在值真的变化时写入新版本。每次部署无脑追加版本，会让版本数随部署频率
线性增长直到撞上配额，更糟的是摧毁审计线索：当每次部署都产生新版本时，没有任何
东西能把"真的轮换了口令"和"例行部署"区分开。

### Vault 路径为什么是 platform 作用域

`kv/data/<env>/platform/smtp/google`，而非 `.../accounts/smtp/google`。

这个邮箱身份不属于 accounts 服务——billing-service 的催缴邮件规划用的是同一个
身份。按服务分路径意味着同一个应用专用密码存两份、轮换两次，最终变成只轮换了一次。

### 组件职责

| 组件 | 职责 | 禁止事项 |
| --- | --- | --- |
| Vault | 按环境保存 SMTP 用户名与应用专用密码，是唯一真源 | 把凭据同时保存在别处，造成多个轮换点 |
| 同步脚本 | 把 Vault 的值投递进 Secret Manager，值未变时不写新版本 | 用 `--update-env-vars` 注入口令——那会让它明文留在 service spec 里，任何能跑 `gcloud run services describe` 的人都读得到 |
| Cloud Run | 通过 `secretKeyRef` 引用机密 | 在 manifest 里写入任何机密字面量 |
| accounts | 渲染模板、发信、把失败翻成 API 错误 | 在 SMTP 四要素缺失时假装成功而不留痕迹 |

### TLS 模式钉死为 starttls

配置中 `smtp.tls.mode` 显式设为 `starttls`，不用 `auto`。

`auto` 在对端未广播 STARTTLS 时会**静默降级为明文**，而 Go 的 `smtp.PlainAuth`
拒绝在未加密连接上发送凭据——最终报错与"密码错误"无法区分。钉死之后，握手阶段
就会失败，错误指向真正的原因。

### 失败如何呈现

发送是同步的，10 秒硬超时。失败翻成 `500 verification_failed` 或
`504 smtp_timeout`，portal 再映射成前端提示。

**一个容易误判的分叉**：当 SMTP 的 host / username / password / from 任一为空时，
`emailVerificationEnabled` 被置为 false，`/auth/register/send` 直接返回 200
「verification email sent」**而不发信**。所以：

- 页面报错 → 凭据是配上的，投递真的失败
- 页面成功但收不到 → 凭据缺失被静默降级，或 DNS 认证未过被收件方丢弃

两者的排查方向完全相反。

## 发件身份

`SMTP_FROM` 为 `XWorkmate <no-reply@xworktech.com>`，而 `SMTP_USERNAME` 是
`haitaopan@xworktech.com`。两者不同是**合法且有意**的。

Google Groups 无法进行 SMTP 认证——组没有密码，也生成不了应用专用密码——所以
认证账号必须是真实用户。`no-reply` 是该用户的**邮箱别名**（每用户 30 个，免费、
不占席位）。

Gmail 的规则是 envelope sender 必须是认证账号**或其已验证别名**，别名满足后者。
不满足时返回 `550-5.7.1 not allowed to send as this address`。

`svc.plus` 是 `xworktech.com` 的**用户别名域**，因此 `no-reply@svc.plus` 自动
成立、无需额外配置，也不额外占用席位。要把发件人切到该域，只需改 `SMTP_FROM`
一行，`SMTP_USERNAME` 保持不变——前提是 svc.plus 自己的 DKIM 已经生成（见下）。

非生产环境与生产共用同一发信身份，靠**显示名**区分：preview 的 `SMTP_FROM` 为
`XWorkmate (Preview) <...>`。收件人收到一封自己没申请过的验证码时，显示名是唯一
能判断它来自测试部署的线索。

## DNS 平面

| 层 | 仓库 | 位置 | 职责 |
| --- | --- | --- | --- |
| 声明 | gitops | `resources/<zone>/<env>/cloudflare/email-dns.yaml` | 期望状态：MX、SPF include、DKIM selector、DMARC |
| 对账 | playbooks | `configure_email_dns.yml` | Ansible 实现，按 name/type 对做差异 |
| 调度 | platform-ops-toolkit | `.github/workflows/configure-email-dns.yaml` | 唯一入口，zone 串行、不 fail-fast |
| 凭据 | Vault | `kv/data/<env>/xworktech-email` | `cloudflare_api_token`，需 Zone:Read + DNS:Edit |

调度层在 toolkit 而非 playbooks：后者是 Ansible 内容层，其他跑这些 playbook 的
流水线全部从 toolkit 驱动。**Vault role 绑定的是仓库的 OIDC subject**，所以这次
搬迁也改变了读 Cloudflare token 的角色——
`github-actions-platform-ops-toolkit-<env>` 需要被授予该路径的读权限，否则运行
停在 Vault 步骤报一个点名该路径的 403。

zone 之间串行且不 fail-fast：两份 policy 编辑的是同一个 Cloudflare 账号，且每个
zone 的 apex SPF 是**整条改写**而非差异更新，一个 zone 失败不能让另一个停在半
应用状态。

### 域名与 DKIM

两个 zone 都在声明范围内：`xworktech.com`（主域）与 `svc.plus`（用户别名域）。

**域名别名不继承父域的 DKIM 密钥。** 每个 zone 各有自己的 selector，密钥对只能在
Admin 控制台生成（Apps → Google Workspace → Gmail → Authenticate email），DNS
侧造不出来。公钥是唯一进入 DNS 的那一半，属于非敏感配置，因此写在 GitOps 声明里
而不是 Vault。

另需注意：**DKIM 的 TXT 记录存在 ≠ 签名已开启**。控制台里的状态要是"正在验证
邮件"而不是"开始验证"。

### 对账器刻意不做的三件事

**不接管 apex TXT 名字。** 那个名字上住着合并后的 SPF，以及域名自带的验证
token（Search Console、Workspace 等）。按名字整体清空会把它们一并带走。apex TXT
被排除在"本对账器独占"的集合之外；若有声明的记录落到那里，运行**直接失败**而不是
安静跳过。

**绝不发布第二条 `v=spf1`。** apex SPF 由声明的各 provider 合并成单条记录写入。
两条 `v=spf1` 会让收件方直接判定 SPF 失败——比没有 SPF 更糟。

**不发布未生成的 DKIM。** `content` 为空表示密钥还没在 Admin 控制台生成。发布它
等于在 DNS 里留下一个坏 selector。对账器跳过并打印提示，说明该去哪里生成。

### 期望的记录形态

```text
apex SPF   v=spf1 include:_spf.google.com ~all
MX         aspmx.l.google.com 1 · alt1/alt2 5 · alt3/alt4 10
DKIM       google._domainkey.<zone>   每个 zone 独立生成
DMARC      v=DMARC1; p=none; rua=mailto:security@xworktech.com; adkim=s; aspf=s
```

## 邮件内容

三封邮件共用 `api/email_template.go` 的同一套模板：表格布局 + 全内联样式（Gmail
剥离 `<style>` 块，Outlook 走 Word 渲染引擎）。

高亮码有两种排版：6 位验证码大号等宽加字距单行居中；64 位 hex 重置 token 必须
换行，套用前者会在移动端客户端溢出。

**纯文本部分保持纯 ASCII。** 它以 `Content-Transfer-Encoding: 7bit` 发出，塞入
中点等非 ASCII 字符等于声明 7bit 却发送 8bit 字节，部分中继会直接拒收。此类标点
只出现在 quoted-printable 编码的 HTML 部分。

## 验收标准

不是"收到了"。在原始邮件头中确认：

```text
Authentication-Results: ... spf=pass ... dkim=pass ... dmarc=pass
```

三项全 pass 才算完成。只要一项 fail，邮件现在能收到，但迟早会进垃圾箱——而那时
不会有人把问题和这次变更联系起来。

## 已知债务

**与 `roles/cloudflare_dns` 的重复实现。** playbooks 仓库里已有通用 Cloudflare
DNS 对账 role（消费方 `update_site_dns.yml` 等四处），而 email 这条线自己用 `uri`
任务重写了一遍。收敛前有两个硬缺口：该 role 的创建 body 不含 `priority`，表达
不了 MX；删除只按 name 不带 type，用在 zone apex 上会连同网站的 A 记录一起删掉。
收窄成按 type 会改变现有消费方行为（同名 A→CNAME 替换会残留），需要加作用域开关
而非一刀切。

**Vault 路径按环境切 vs 单一轮换点。** 三个环境共用同一发信身份，却各存一份，
轮换要改三处。收敛成 `kv/data/platform/smtp/google` 可得到单一轮换点，但需给三个
env role 各加一条读授权（`kv/data/CICD` 是这种"三环境共读"的先例）。当前选择了
不需要 Vault 侧改动的那条。

## 相关文档

| 内容 | 位置 |
| --- | --- |
| 上线操作步骤与验证 | `docs/Runbook/Deploy-Email-Delivery-Bootstrap.md` |
| SMTP 配置细则与发件身份 | `docs/SMTP_GMAIL_SETUP.md` |
| DNS 对账器与其拒绝做的事 | playbooks `docs/email-dns.md` |
| DNS 声明 | gitops `resources/<zone>/<env>/cloudflare/email-dns.yaml` |
