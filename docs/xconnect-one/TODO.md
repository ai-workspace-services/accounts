# XConnect-One Accounts TODO

状态日期：2026-08-28

## P0：真实数据库验证

- [ ] 启动临时 PostgreSQL，按顺序执行 Batch 07/08 migration 和 down/up 恢复演练。
- [ ] 在真实事务与唯一索引上覆盖 concurrent join/import/upsert/rotate key claim、单设备
  单 active credential、rotation 丢响应恢复、revoke receipt 原样重放和 session 失效。
- [ ] 验证最小权限 DB role、备份/恢复、migration 锁时长、审计脱敏和失败重试；保存命令、
  版本、结果和清理记录。

## P0：SignedConfig v2 producer

- [ ] 只在显式 `Accept: application/vnd.xconnect.signed-config.v2+json` 下返回 v2；缺省
  Accept 保持严格 v1，未知版本返回明确错误，不静默降级。
- [ ] v1/v2 都设置 `Vary: Accept` 和 `Cache-Control: private, no-store`。
- [ ] 将 policy generation、digest、同源相对 path 和 media type 纳入 Ed25519 签名，
  签名字节与 IAC v2 golden 完全一致。
- [ ] 实现 `/api/overlay/v1/enrollment/policy-artifacts/{generation}/{digest}`，只接受
  config-read 短 token，拒绝跨设备/网络、过期 generation 和 digest 不一致。
- [ ] 与 XConnect-APP Batch 08 consumer 联调 v1 默认、v2 opt-in、redirect、media type、
  digest、replay floor 和原子 apply/readback/ACK。

## P0：cutover authorization 生产化

- [x] Batch 09 producer、严格 binding、service-token 鉴权、独立签名和 fail-closed 测试
  已完成并推送至 `98edbbe`。
- [ ] 建立 signer 私钥托管、根保护公钥分发、current/previous key rotation、审计和紧急
  撤销 runbook；生产环境不得使用测试 seed。
- [ ] 与 Playbooks Gateway Batch 06 做真实 HTTPS 联调，覆盖篡改、过期、重放、错误
  node/network/generation、pending reconcile、错误 baseline 和 signer rotation。
- [ ] 连续收集 import receipt、snapshot/policy、成功 apply、runtime readback 和健康样本；
  staging soak 通过前不授权 production accounts-only。

## P1：跨仓与发布

- [ ] 建立真实租户，覆盖 join、sync、ACL allow/deny、rotate、suspend、revoke、leave、
  Gateway rollback、控制面短暂不可用和证书轮换。
- [ ] 确认终端 Join/Leave 只更新 Accounts 投影，不反写或删除静态 `group_vars`。
- [ ] 输出 API/CLI/Gateway 兼容矩阵、SLO/告警、备份恢复和 operator runbook。
- [ ] v1 始终只输出 `proxy_core: xray`；不增加 sing-box schema、runtime 或 fallback。

## PR

- [ ] 当前没有新 PR。先核对远端长期分支和已有 PR，再按可审查批次提交到
  `codex/xconnect-overlay-productization`；实际创建前需再次取得用户确认。
