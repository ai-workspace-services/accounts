# OAuth 注册登录(GitHub / Google)

> **Status**: GitHub ✅ + Google ✅ 均已上线并验证 307
> **Date**: 2026-07-11
> **Owner**: haitaopan
> **Related PRs**: accounts [#12](https://github.com/ai-workspace-services/accounts/pull/12) [#13](https://github.com/ai-workspace-services/accounts/pull/13) [#14](https://github.com/ai-workspace-services/accounts/pull/14) [#16](https://github.com/ai-workspace-services/accounts/pull/16) · playbooks [#111](https://github.com/ai-workspace-infra/playbooks/pull/111) [#112](https://github.com/ai-workspace-infra/playbooks/pull/112)
> **Deploy target**: install.svc.plus = jp-xhttp-contabo.svc.plus (46.250.251.132),VPS docker compose,容器 `accounts`

## 目标

`https://accounts.svc.plus/api/auth/oauth/login/github` 返回 404 → 补全 GitHub(及 Google)OAuth 注册登录,任意账号可开放注册(无白名单),首登自动建号 + 7 天 trial。

## 结论

- **GitHub 已端到端上线**:`login/github` 返回 307 → GitHub 授权页(client_id/redirect_uri/scope 正确,frontend_url 经 state base64 回填 console.svc.plus);浏览器闭环实测登录成功(用户 haitaopan 拿到 UUID + MFA)。
- 代码链路本就完整;404 根因是**部署链未注入 OAuth 密钥**,非代码缺陷。
- Google **代码/部署链已就绪**,只差 Google Cloud 凭证 + Vault/secret + 合并 #16。

## PR 明细

### accounts #12 [MERGED] — Enable GitHub OAuth login: deploy env passthrough + setup docs
- pipeline.yml 透传 `OAUTH_GITHUB_CLIENT_ID`(后改为 repo var)+ `OAUTH_GITHUB_CLIENT_SECRET` / `AUTH_TOKEN_*` secrets
- 新增 `docs/OAUTH_GITHUB_SETUP.md` 全量接入指南
- account.cloudrun.yaml / prod-service.yaml 同步(Cloud Run 路线,当前未启用)
- ⚠️ 合并时 gitleaks gate 是红的(带入历史泄漏),由 #13 补救

### accounts #13 [MERGED] — Fix gitleaks gate after OAuth merge
- client_id 硬编码 → `vars.OAUTH_GITHUB_CLIENT_ID`(消除 generic-api-key 命中)
- 清除 runbook 里泄漏的 bearer token `uTvry…`,历史指纹进 `.gitleaksignore`
- 合并提交 df57410 的公开 client_id 也 baseline
- 用 gitleaks 8.21.2(与 CI 同版)本地验证 no leaks

### accounts #14 [MERGED] — Enforce email blacklist on OAuth login
- **安全修复**:管理员邮箱黑名单原本只在密码 register/login 生效,`oauthCallback` 不查 → 被封邮箱可经 OAuth 绕过。加 `IsBlacklisted` 检查(403 `email_blacklisted`)
- 实现 MemoryStore 的黑名单方法(原为空 stub),补测试 `TestOAuthCallbackRejectsBlacklistedEmail`
- 遗留(out-of-scope):paused(`Active=false`)用户走 OAuth 仍能拿 session,后续 API 被 `RequireActiveUser` 拦但登录本身没挡 → 见下方待办

### accounts #16 [MERGED] — Wire Google OAuth secrets into the deploy pipeline
- pipeline.yml 增 `OAUTH_GOOGLE_CLIENT_ID`(var)+ `OAUTH_GOOGLE_CLIENT_SECRET`(secret)透传
- Google 凭证:GCP 项目 xzerolab-480008 建 Web OAuth client,client_id `266500572462-c00141…apps.googleusercontent.com`,callback `…/callback/google`
- 已设 GH secret/var + Vault `OAUTH_GOOGLE_CLIENT_SECRET`;合并部署后 `login/google` 验证 307 通过
- Vault 顺手改名 `GITHUB_CLIENT_SECRET` → `OAUTH_GITHUB_CLIENT_SECRET`,与 Google 命名对齐(纯备份字段,零运行时影响)

### playbooks #111 [MERGED] — Wire GitHub/Google OAuth + auth token env into role
- `app.env.j2` / `defaults/main.yml` / `target.yml`(lineinfile,覆盖存量主机)增 `AUTH_TOKEN_*` `GITHUB_CLIENT_*` `GOOGLE_CLIENT_*`
- `account.yaml.j2` 去掉 envsubst 不支持的 `${VAR:-default}`(该写法导致线上 JWT 密钥曾是字面量垃圾串);redirectUrl 拆 per-provider
- CI 命名用 `OAUTH_GITHUB_*` / `OAUTH_GOOGLE_*`(Actions 禁 `GITHUB_` 前缀),role 映射回容器内 `GITHUB_CLIENT_*` / `GOOGLE_CLIENT_*`

### playbooks #112 [MERGED] — Fix accounts deploy: guard ansible_os_family
- `gather_facts:false` 下 `accounts_service_caddy_base_dir` 解引用未定义的 `ansible_os_family` → 部署过 SSH 后炸。加 `| default('')` 回落 Linux caddy 路径

## 部署踩坑实录(过 SSH prep 后连环三坑)

1. **caddy `ansible_os_family` undefined**(playbooks#112 修)
2. **prod 树被 `chattr +i` 锁死**:`/opt/cloud-neutral/accounts/managed/prod` 整树 immutable(5/30 手动加,role 不管理),root 也写不了 `app.env`。`chattr -R -i` 解锁。⚠️ 若属有意硬化,需改造为部署后置步骤
3. **caddy 片段重名冲突**:role 写 `conf.d/accounts.caddy`,与旧 `conf.d/accounts.svc.plus.caddy`(仅多死别名 accounts-contabo-e700175)并存 → `ambiguous site definition`。旧文件 mv 到 `/root/*.bak`

## 密钥 / 凭证映射

| 用途 | Vault | GH Secret/Var | GitHub 值 |
|---|---|---|---|
| GitHub client_id | — (公开) | var `OAUTH_GITHUB_CLIENT_ID` | `Ov23lioecyD2bjWNQJr0` |
| GitHub secret | `kv/accounts.svc.plus:GITHUB_CLIENT_SECRET` | secret `OAUTH_GITHUB_CLIENT_SECRET` | — |
| JWT 三密钥 | `kv/accounts.svc.plus:AUTH_TOKEN_*` | secret `AUTH_TOKEN_{PUBLIC_TOKEN,REFRESH_SECRET,ACCESS_SECRET}` | — |
| SSH 部署私钥 | `kv/CICD:SINGLE_NODE_VPS_SSH_PRIVATE_KEY`(= shenlan RSA `Osewib…`,在目标机 authorized_keys) | secret `SINGLE_NODE_VPS_SSH_PRIVATE_KEY` | — |
| Google client_id | — | var `OAUTH_GOOGLE_CLIENT_ID` | 待建 |
| Google secret | `kv/accounts.svc.plus:GOOGLE_CLIENT_SECRET`(待加) | secret `OAUTH_GOOGLE_CLIENT_SECRET` | 待建 |
| bridge 服务间 token | `kv/accounts.svc.plus:INTERNAL_SERVICE_TOKEN` | — (2026-07-12 改走 `hashicorp/vault-action` OIDC role `github-actions-accounts`,不再落 GH secret) | — |
| 评审账号 bridge token | `kv/accounts.svc.plus:BRIDGE_REVIEW_AUTH_TOKEN` | — (同上,OIDC role 直读) | — |

- accounts 服务运行时用来读 `xworkmate/*` 的 `XWORKMATE_VAULT_TOKEN` **不**经这条 pipeline 或任何 GH secret 管理,只写在部署主机 `app.env`,需要轮换时手工改主机文件 / Vault UI(见 README「CI/CD 部署前置条件」一节)。2026-07-12 曾尝试让 pipeline 用 GH secret 顺带轮换它,已撤销 — CI 只负责 `INTERNAL_SERVICE_TOKEN` / `BRIDGE_REVIEW_AUTH_TOKEN` 这两个部署期 token。

- SSH 私钥坑:PEM 缺尾换行,本地 `ssh-keygen` 报 invalid format,CI `normalize-private-key.py` 会补。
- callback URL:GitHub/Google 均指向 accounts 后端 `…/api/auth/oauth/callback/{provider}`。

## 遗留待办

- [x] **Google 上线** ✅(2026-07-11):#16 已合并部署,`login/google` 307 通过。⚠️ **待确认**:Google consent screen 是否已切 "In production" —— 若停在 Testing 则仅 test users 可登(email/profile 非敏感 scope,发布无需 verification)
- [ ] **轮换 Google client_secret**:`GOCSPX-…` 已明文出现在对话,Console Reset 一次消除泄漏面
- [ ] **paused 用户全功能锁死**:`Active=false` 用户可登录但锁所有功能。收口点 = 共享 helper `requireAuthenticatedUser`(api.go:1516)加 Active 检查,覆盖无中间件的 `accountGroup`/`agentServerGroup`/`agentGroup`(其余组已有 `RequireActiveUser`)。**未开 PR**
- [ ] immutable 锁决策:是否改造成部署收尾再锁
- [ ] caddy 片段命名对齐:role `accounts.caddy` → `accounts.svc.plus.caddy`(可选)
- [ ] SSH 排查文档(commit 8e41ad1,在已合分支 oauth-gitleaks-cleanup)未进 main,需补 PR
- [ ] 安全:Vault root token(泄漏于 `cloud-neutral-toolkit/openclaw-deploy-example/init.json`,已加 .gitignore)建议轮换

## 遗留代码缺陷(非本任务引入,记录备查)

- oauthCallback CSRF state 未校验(生成 nonce 但 callback 不比对)
- portal `frontend_url` 用 `request.nextUrl.origin`,容器内直连得到容器 hostname
- oauth 仅按 email 匹配用户,未用 `(provider, external_id)` identity 表 → 用户改 GitHub 主邮箱会被当新用户
