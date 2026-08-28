# XConnect-One Accounts 实施更新记录

状态日期：2026-08-28

本仓职责：设备与网络事实源、邀请/凭据、签名配置、Gateway 投影、动态 ACL 和切换授权

## 不变量

- XConnect-One v1 的 `proxy_core` 只允许 `xray`，对应 Xray-core/libXray；不实现
  sing-box runtime 或 fallback。
- Accounts 保存控制面事实并产生签名投影，不接收 WireGuard private key、原始设备凭据、
  refresh token 或签名 private key 的日志副本。
- 动态 ACL 默认拒绝。用户、邮箱、组和 tag owner 停留在管理边界，Gateway artifact
  只包含展开后的设备标识。
- 静态 `group_vars` 仍是迁移/回滚证据，本仓不反向生成或删除该清单。

## 已完成并推送的批次

| 批次 | 远端分支 | SHA | 结果 |
|---|---|---|---|
| 01 | `codex/xconnect-batch-01-overlay-contracts` | `01b8093` | Overlay v1 API 与领域合同 |
| 02 | `codex/xconnect-batch-02-signed-config-projection` | `449e5f0` | SignedConfig v1 producer |
| 03 | `codex/xconnect-batch-03-projection-persistence` | `f68344e` | generation、ACK 和投影持久化 |
| 04 | `codex/xconnect-batch-04-invite-enrollment` | `6056a98` | 一次性邀请、Join 和设备注册 |
| 05 | `codex/xconnect-batch-05-gateway-projection` | `5c4a6ed` | 签名 GatewaySnapshot 投影 |
| 06 | `codex/xconnect-batch-06-acl-compiler` | `30c288e` | 确定性 default-deny ACL compiler |
| 07 | `codex/xconnect-batch-07-device-lifecycle` | `d7e2258` | 生命周期、reconcile、apply 与永久 key tombstone |
| 08 | `codex/xconnect-batch-08-device-session` | `14a79e7` | 耐久设备凭据、短期 session、轮换、撤销与 migration |
| 09 | `codex/xconnect-batch-09-cutover-authorization` | `98edbbe` | 独立 Ed25519 accounts-only cutover authorization producer |

### Batch 07/08 安全边界

- `(network_id, wireguard_public_key)` 的历史占用永久保留，register、join、静态导入、
  Upsert 和 rotate 在事务中 claim，阻止撤销 key、轮换旧 key 和并发回滚重用。
- Join 只返回一次 canonical `xdc_<id>.<secret>`；数据库仅保存完整 token UTF-8 的
  SHA-256 verifier、binding、scope、状态、到期和 replacement chain。
- `Authorization: Device` 只能 mint 最长 15 分钟、仅 config read/ACK scope 的 `xenr_`
  session；设备凭据不能直接读取配置。
- rotation 由客户端生成 successor，Accounts 只接收 id 与 verifier。device-bound revoke
  原子终止设备、session 和 enrollment，并以 nonce/body hash 返回可重放终态 receipt。
- session 返回当前/前任公开 Ed25519 signing-key ring，不返回 private key。

### Batch 09 切换授权边界

- endpoint 受内部 service token 保护，签发器使用独立 Ed25519 key，不把本地 JSON 布尔值
  当作 controller approval。
- 授权严格绑定 Gateway Batch 06 的 canonical node/network/generation/snapshot、import
  baseline、Accounts projection、policy digest、reconcile counters、requested mode 和时窗。
- signer/store/snapshot、成功 apply、import baseline 任一缺失，或存在 pending reconcile，
  都拒绝签发。

## 验证证据

- Batch 08：目标 Go test、`go test -race ./internal/store ./api`、`go vet ./...`、
  OpenAPI/YAML/JSON、合同向量、迁移结构、并发/重放和日志脱敏测试通过。
- PostgreSQL 事务与锁目前主要使用 sqlmock 验证；这不是实际 PostgreSQL 进程烟测。
- Batch 09：canonical signing golden、HTTP 鉴权/fail-closed、schema、OpenAPI、migration
  index、audit、race/vet/parse 测试通过。
- 仓库全量 Go 测试只有既有 overlayctl E2E 因本机缺少 `wg` 命令失败；未把该环境缺口
  记录为功能通过。

## 当前边界

- SignedConfig v2 producer 和同源 policy artifact endpoint 尚未实现；XConnect-APP 的
  v2 consumer 已在其 Batch 08 完成，但还没有本仓 producer 的 live 联调证据。
- cutover authorization producer 已完成代码和自动化验证，尚未在真实 Accounts →
  Gateway 环境部署、轮换根信任、执行连续健康 soak。
- 本仓没有真实 PostgreSQL 并发/回滚烟测，也没有 Accounts + Gateway + 五平台 live E2E。
- 当前批次仍在特性分支，未创建新的 PR；后续 PR base 为
  `codex/xconnect-overlay-productization`，创建前需再次取得用户确认。
