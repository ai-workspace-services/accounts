# Runbook 目录

本目录包含该项目的运维手册和故障排查文档。

## 📚 文档分类

### 🔧 部署相关
- [Deploy Email Delivery Bootstrap](./Deploy-Email-Delivery-Bootstrap.md) - 启用 XWorkmate 事务邮件通道（Google Workspace SMTP + 域名 DNS + Vault 凭据）。
- 部署流程
- 环境配置
- 依赖管理

### 🚨 故障排查
- 常见问题
- 错误诊断
- 应急处理

### 🔄 运维操作
- [Security Scrubbing Archive](./Security-Scrubbing-Archive-2026-02-06.md) - 历史敏感信息深度脱敏记录。
- [Fix Rotating UUID Sync Archive](./Fix-Rotating-UUID-Sync-Archive-2026-02-06.md) - P1 级别 Sandbox 旋转 UUID 同步故障修复记录。
- [Fix Agent 404 and UUID Change](./Fix-Agent-404-And-UUID-Change.md) - 解决代理 API 认证 404 及 UUID 变更问题。
- [Fix CloudRun Stunnel Startup Failure](./Fix-CloudRun-Stunnel-Startup-Failure.md) - 解决 CloudRun 环境下 Stunnel 启动失败问题。
- 日常维护
- 备份恢复
- 性能优化

### 📊 监控告警
- 监控指标
- 告警规则
- 日志分析

## 📝 文档规范

每个 Runbook 应包含：

1. **问题描述**: 清晰描述问题现象
2. **影响范围**: 说明影响的功能和用户
3. **诊断步骤**: 详细的问题定位方法
4. **修复方案**: 具体的解决步骤
5. **验证方法**: 确认问题已解决的检查清单
6. **回滚计划**: 如果修复失败的应急方案

## 🎯 命名规范

- 使用描述性的文件名
- 格式: `[类型]-[简短描述].md`
- 示例: `Deploy-Database-Migration.md`, `Fix-API-Timeout.md`

## 📅 维护说明

- 定期更新文档
- 记录最后更新时间
- 标注负责人和审核人

---

## 📂 归档记录 (Archives)

### 2026-02-06
- [Security Scrubbing Archive](./Security-Scrubbing-Archive-2026-02-06.md) - 历史敏感信息深度脱敏记录。
- [Fix Rotating UUID Sync Archive](./Fix-Rotating-UUID-Sync-Archive-2026-02-06.md) - P1 级别 Sandbox 旋转 UUID 同步故障修复记录。
