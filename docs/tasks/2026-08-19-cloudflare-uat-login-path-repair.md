# Cloudflare UAT 登录链路修复 —— accounts 视角（CORS Origin 白名单）

> **Status**: 🟡 代码已提交，待合并 + 重新部署 Cloud Run
> **Date**: 2026-08-19
> **Related PRs**: 本仓 PR 待填 · 配套 platform-ops-toolkit / edge-gateway / portal 各一
> **完整跨仓规划**: `platform-ops-toolkit/docs/tasks/2026-08-19-cloudflare-uat-login-path-repair.md`

## accounts 承担的部分（P0 主因）

`https://console-cloudflare-uat.onwalk.net` 上**每一次浏览器登录都被 accounts 的
CORS 中间件 403 掉**，用户侧表现为「登录失败，请稍后再试。」。

同一请求只差一个 header：

| 请求 | 结果 |
|---|---|
| `POST /api/auth/login`（无 Origin） | `401 application/json {"error":"invalid_credentials"}` |
| 同上 + `Origin: https://console-cloudflare-uat.onwalk.net` | **`403 text/html`，content-length 0** |
| 同上 + 仅 `Referer` / 仅浏览器 `User-Agent` | 401 JSON（不触发） |

空 body 的 403 是 gin-contrib/cors `AbortWithStatus(403)` 的签名。白名单在
`cmd/accountsvc/main.go` 的 `AllowOriginFunc` + `resolveAllowedOrigins()`，
数据来自 `config/account.cloudrun.yaml` 的 `allowedOrigins` —— 其中只有
`https://console.svc.plus`，**没有任何 `*-cloudflare-*.onwalk.net`**。
Cloud Run 部署用的正是该文件（`CONFIG_TEMPLATE=/app/config/account.cloudrun.yaml`），
部署脚本此前不传任何 origin 相关 env。SIT 同病，生产不受影响。

因为 edge-gateway 自己回了 `Access-Control-Allow-Origin: *`，浏览器不报 CORS 错，
只表现为一个"应用错误" —— 这是它藏了这么久的原因。**不带 Origin 的 curl 探测
永远发现不了它。**

## 本次改动

`resolveAllowedOrigins()` 增加读取 **`ALLOWED_ORIGINS`** 环境变量（逗号分隔），
与配置文件里的 `server.allowedOrigins` **合并**（不是替换），复用既有的
`parseOrigin` 归一化、`*` 通配与去重逻辑；空值与空段自动跳过。

规划里原本还提了在 yaml 里加 `"${CONSOLE_PUBLIC_ORIGIN}"` 模板槽（`entrypoint.sh`
已做 `envsubst`）。**最终只实现环境变量一条路径** —— 两个开关做同一件事，正是本次
故障的成因（同一份知识多处副本），没有必要再造一份。

值由 platform-ops-toolkit 的 Cloud Run 部署脚本从 GitOps `EdgeRoutingConfig` 的
`spec.serverless.console_host` **推导**后传入，因此新环境上线时不需要再改这个仓。

## 验收

```bash
curl -sS -o /dev/null -w '%{http_code} %{content_type}\n' \
  -X POST https://console-cloudflare-uat.onwalk.net/api/auth/login \
  -H 'Content-Type: application/json' \
  -H 'Origin: https://console-cloudflare-uat.onwalk.net' \
  -d '{"identifier":"nobody","password":"nobody"}'
```

期望 `404`/`401` + `application/json`，不再是 `403 text/html`（空 body）。
生产 `console.svc.plus` 行为需回归验证无变化。

## 遗留

前端错误码映射不全是独立缺陷（P1-2）：`email_not_verified`、`account_suspended`、
`sandbox_no_login`、`password_required` 等在 portal 全部无映射，一律显示
「登录失败，请稍后再试。」。计划从本仓 `api/*.go` 的 `respondError` 生成 TS 联合类型。
