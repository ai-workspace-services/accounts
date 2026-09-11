# Cloud Run Stunnel Sidecar 启动失败导致服务无法启动

**类型**: 故障排查  
**严重级别**: P1 (Critical)  
**最后更新**: 2026-01-28  
**负责人**: SRE Team

---

## 📋 问题描述

Cloud Run 部署 `accounts-svc-plus` 服务时，容器启动失败并报错：

```
ERROR: (gcloud.run.services.update) The user-provided container failed to start 
and listen on the port defined provided by the PORT=8080 environment variable 
within the allocated timeout.
```

**错误日志链接示例**:
```
https://console.cloud.google.com/logs/viewer?project=$GCP_PROJECT&resource=cloud_run_revision/service_name/accounts-svc-plus/revision_name/accounts-svc-plus-00049-gjv
```

---

## 🎯 影响范围

- **服务**: `accounts-svc-plus` (账号服务)
- **影响功能**: 
  - 用户登录/注册
  - 账号管理 API
  - 所有依赖账号服务的下游系统
- **影响用户**: 全部用户
- **持续时间**: 直到修复完成

---

## 🔍 根因分析

### 架构背景
该服务使用 **Sidecar 模式** 部署：
- **主容器**: `accounts-api` (Go 应用)
- **Sidecar 容器**: `stunnel-sidecar` (TLS 隧道，用于连接远程 PostgreSQL)

### 问题链路
1. **Stunnel 配置问题**:
   - `stunnel.conf` 配置中指定 PID 文件路径为 `/var/run/stunnel/stunnel-account-db-client.pid`
   - Sidecar 容器 (`dweomer/stunnel`) 中该目录不存在或无写权限
   - Stunnel 进程启动失败

2. **主容器启动依赖**:
   - `entrypoint.sh` 脚本检测 `DB_HOST:DB_PORT` (127.0.0.1:15432) 是否可达
   - Stunnel 未启动 → 15432 端口未监听
   - 主应用尝试连接数据库失败 → 进程退出

3. **Cloud Run 健康检查**:
   - `startupProbe` 检测 8080 端口 TCP 连接
   - 主应用未启动 → 健康检查失败
   - Cloud Run 判定容器启动失败

### 配置文件位置
- **Stunnel 配置**: `deploy/gcp/cloud-run/stunnel.conf`
- **Secret 管理**: Google Secret Manager (`stunnel-config`)
- **Service YAML**: `deploy/gcp/cloud-run/service.yaml`

---

## 🛠️ 诊断步骤

### 1. 查看 Cloud Run 日志
```bash
gcloud logging read "resource.type=cloud_run_revision \
  AND resource.labels.service_name=accounts-svc-plus" \
  --limit 50 --format json --project "$GCP_PROJECT"
```

**关键错误信息**:
- `stunnel: Cannot create pid file`
- `nc: connect to 127.0.0.1 port 15432 (tcp) failed: Connection refused`
- `stunnel not ready after 30s`

### 2. 检查 Stunnel 配置
```bash
# 查看当前 Secret 版本
gcloud secrets versions list stunnel-config --project "$GCP_PROJECT"

# 查看配置内容
gcloud secrets versions access latest --secret=stunnel-config \
  --project "$GCP_PROJECT"
```

### 3. 本地复现（可选）
```bash
# 拉取 Sidecar 镜像
docker pull dweomer/stunnel

# 测试配置
docker run --rm -v $(pwd)/deploy/gcp/cloud-run/stunnel.conf:/etc/stunnel/stunnel.conf \
  dweomer/stunnel stunnel /etc/stunnel/stunnel.conf
```

---

## ✅ 修复方案

### 步骤 1: 修改 Stunnel 配置

编辑 `deploy/gcp/cloud-run/stunnel.conf`:

```diff
 ; Stunnel configuration for Cloud Run (client mode)
-pid = /var/run/stunnel/stunnel-account-db-client.pid
-output = /var/run/stunnel/stunnel-account-db-client.log
+pid = /tmp/stunnel.pid
+# output = /dev/stdout
 foreground = yes
```

**修改说明**:
- `/tmp` 目录在所有容器中都可写
- 注释掉 `output` 配置，默认输出到 stdout/stderr（Cloud Run 会自动收集）
- `foreground = yes` 确保进程不会后台运行（Cloud Run 要求）

### 步骤 2: 更新 Secret
```bash
cd /path/to/accounts.svc.plus

# 更新 Secret（会创建新版本）
make cloudrun-stunnel GCP_PROJECT="$GCP_PROJECT"

# 或手动执行
gcloud secrets versions add stunnel-config \
  --data-file deploy/gcp/cloud-run/stunnel.conf \
  --project "$GCP_PROJECT"
```

### 步骤 3: 重新部署服务
```bash
# 触发新部署（会拉取最新 Secret 版本）
make cloudrun-deploy GCP_PROJECT="$GCP_PROJECT"

# 或手动执行
gcloud run services replace deploy/gcp/cloud-run/service.yaml \
  --region asia-northeast1 \
  --project "$GCP_PROJECT"
```

---

## 🧪 验证方法

### 1. 检查部署状态
```bash
gcloud run services describe accounts-svc-plus \
  --region asia-northeast1 \
  --project "$GCP_PROJECT" \
  --format="value(status.conditions)"
```

**预期输出**: `Ready: True`

### 2. 测试健康检查
```bash
SERVICE_URL=$(gcloud run services describe accounts-svc-plus \
  --region asia-northeast1 \
  --project "$GCP_PROJECT" \
  --format="value(status.url)")

curl -f "${SERVICE_URL}/healthz"
```

**预期输出**: `{"status":"ok"}`

### 3. 测试登录 API
```bash
curl -X POST "${SERVICE_URL}/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"test@example.com","password":"test123"}'
```

**预期输出**: 返回错误信息（如 `user_not_found`），而非连接超时或 500 错误

### 4. 查看实时日志
```bash
gcloud run services logs read accounts-svc-plus \
  --region asia-northeast1 \
  --project "$GCP_PROJECT" \
  --limit 20
```

**关键成功信息**:
- `Service [postgres-client] accepted connection from 127.0.0.1`
- `s_connect: connected <DB_IP>:443`
- `configured cors`
- `starting account service`

---

## 🔄 回滚计划

如果修复失败，执行以下回滚步骤：

### 方案 A: 回滚到上一个稳定版本
```bash
# 查看历史 Revision
gcloud run revisions list --service accounts-svc-plus \
  --region asia-northeast1 \
  --project "$GCP_PROJECT"

# 回滚到指定版本（替换 REVISION_NAME）
gcloud run services update-traffic accounts-svc-plus \
  --to-revisions REVISION_NAME=100 \
  --region asia-northeast1 \
  --project "$GCP_PROJECT"
```

### 方案 B: 临时禁用 Stunnel（仅测试环境）
修改 `deploy/gcp/cloud-run/service.yaml`，移除 `stunnel-sidecar` 容器，并将数据库连接改为直连（需配置 Cloud SQL Proxy 或公网访问）。

---

## 📚 相关文档

- [Cloud Run Troubleshooting Guide](https://cloud.google.com/run/docs/troubleshooting)
- [Stunnel Documentation](https://www.stunnel.org/docs.html)
- [Cloud Run Sidecar Pattern](https://cloud.google.com/run/docs/deploying#sidecars)
- [Google Secret Manager](https://cloud.google.com/secret-manager/docs)

---

## 📝 经验总结

### 预防措施
1. **本地测试**: 在部署前使用 Docker Compose 模拟 Sidecar 环境
2. **配置验证**: 添加 CI/CD 步骤验证 `stunnel.conf` 语法
3. **监控告警**: 配置 Cloud Run 启动失败告警（Alerting Policy）

### 最佳实践
- Sidecar 容器的配置文件应使用容器内可写路径（如 `/tmp`）
- 日志输出优先使用 stdout/stderr，便于 Cloud Run 日志聚合
- 主容器启动脚本应设置合理的依赖等待超时（当前为 30s）

### 改进建议
- 考虑使用 Cloud SQL Proxy 替代 Stunnel（官方推荐方案）
- 添加 Stunnel 健康检查端点（如 HTTP status endpoint）
- 在 `entrypoint.sh` 中增加更详细的诊断日志

---

**案例编号**: CASE-2026-01-28-001  
**创建时间**: 2026-01-28 23:19  
**解决时间**: 2026-01-28 23:19  
**总耗时**: ~20 分钟
