# CI 环境路由:main→uat,release/tag→prod

> **Status**: ⏳ 代码完成 + 本地校验通过,待合并(**合并前必须设 `UAT_TARGET_HOST` 变量**)
> **Date**: 2026-07-14
> **Related PRs**: 本 PR(accounts,分支 `ci/uat-prod-env-routing`)

## 目标

修正 pipeline 触发条件并引入环境路由:
- **main** 分支默认部署到 **uat** 环境(持续交付)
- **prod** 环境仅由「push 到 `release/**` 分支」或「push release tag(`v*`)」触发

## 现状(改动前)

- 触发:`push`/`pull_request` 仅 `main`;部署只有单一 `DEFAULT_TARGET_HOST`(= 现 prod 主机),**无 uat/prod 之分**。

## 改动

### 1. 触发器(`.github/workflows/pipeline.yml` `on:`)
```yaml
push:
  branches: [main, "release/**"]
  tags: ["v*"]        # tag push 只匹配 tags 过滤 → prod 专用
pull_request:
  branches: [main]    # 不变,PR 不部署
```

### 2. 环境判定(`scripts/github-actions/resolve-pipeline-flags.sh`)
新增 `deploy_env` 输出,按 `GITHUB_REF` 解析:
| ref | deploy_env |
|---|---|
| `refs/tags/v*` | prod |
| `refs/heads/release/*` | prod |
| `refs/heads/main`(及其它) | uat |
| workflow_dispatch | `INPUT_DEPLOY_ENV`(默认 uat) |

### 3. 主机路由
- prod → `PROD_TARGET_HOST` 变量(默认历史主机 `jp-xhttp-contabo.svc.plus`)
- uat → `UAT_TARGET_HOST` 变量(**必须设置**)
- **安全护栏**:真实部署(push_image && run_apply)时若 `target_host` 为空 → 脚本 `exit 1` 并提示设变量。**防止 main 静默部署到 prod**。
- workflow_dispatch 的 `target_host` 输入可覆盖 env 解析结果。

### 4. GitHub Environment 绑定(deploy job)
`environment: ${{ needs.prep.outputs.deploy_env }}` —— 部署绑定到名为 `uat`/`prod` 的 GitHub Environment。给 **`prod` Environment 加 required-reviewer / 分支保护**即可门禁 release 部署;`uat` 保持持续。

### 5. Stripe key 跟随环境
key pair(sk+whsec,永不混)按 `deploy_env` 选:prod→`PROD_*`,uat→`SANDBOX_*`。`STRIPE_MODE` 变量作显式覆盖(prod 环境跑 sandbox 干跑,或 uat 跑 prod)。

### 6. 其它环境正确性
- `latest` 镜像 tag 只跟 uat/main,release tag/分支不再抢占(部署用 commit-sha tag,不受影响)。
- validate 探针 URL 按 env 选(`vars.PROD_PUBLIC_URL` 默认 `https://accounts.svc.plus`,uat 用 `vars.UAT_PUBLIC_URL`),uat 部署不再被拿去探 prod。

## 本地校验

`resolve-pipeline-flags.sh` 全场景 dry-run + YAML parse 通过:
| 场景 | deploy_env | target_host | push_latest |
|---|---|---|---|
| main push(UAT 已设) | uat | uat.host | true |
| release 分支 push | prod | prod.host | false |
| release tag push | prod | prod.host | false |
| main push(UAT 未设) | — | — | **exit 1(护栏)** |
| dispatch prod override | prod | prod.host | false |
| dispatch uat + host override | uat | manual.host | false |

## 合并前必做(仓库设置)

| 项 | 值 | 必需性 |
|---|---|---|
| variable `UAT_TARGET_HOST` | uat 部署主机/别名 | **必需**(否则 main 部署 exit 1) |
| variable `PROD_TARGET_HOST` | prod 主机 | 可选(默认现有主机) |
| variable `UAT_PUBLIC_URL` | uat 公网 URL | 建议(validate 探针用) |
| variable `PROD_PUBLIC_URL` | prod 公网 URL | 可选(默认 accounts.svc.plus) |
| Environment `prod` | 加 required reviewers | 建议(门禁 release 部署) |
| Environment `uat` | 无保护 | 自动创建即可 |

## 遗留待办

- [ ] 合并前设 `UAT_TARGET_HOST`(及建 uat 主机/inventory)。
- [ ] uat 环境的 playbook inventory / app.env 落点确认(role 复用,主机不同)。
- [ ] `prod` Environment 加 required-reviewer 审批,落实「release 才上 prod」的人工门禁。
- [ ] validate 的 `review-xworkmate-sync` 409(既有问题,另单)与 uat URL 联调。
