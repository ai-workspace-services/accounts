# 配置说明

服务通过 YAML 配置文件运行，示例位于 `config/`：
- `config/account.yaml`
- `config/account-server.yaml`
- `config/account-agent.yaml`
- `config/account.cloudrun.yaml`

> `entrypoint.sh` 会根据 `CONFIG_TEMPLATE` 渲染配置到 `CONFIG_PATH`。

## 顶层字段

```yaml
mode: "server" | "agent" | "server-agent"
log:
  level: info
server: {}
store: {}
session: {}
auth: {}
smtp: {}
xray: {}
agent: {}
agents: {}
```

## server

```yaml
server:
  addr: ":8080"
  readTimeout: 15s
  writeTimeout: 15s
  publicUrl: "https://accounts.svc.plus"
  allowedOrigins:
    - "https://console.svc.plus"
  tls:
    enabled: false
    certFile: ""
    keyFile: ""
    caFile: ""
    clientCAFile: ""
    redirectHttp: false
```

说明：
- `allowedOrigins` 控制 CORS，若为空会回退到 `publicUrl` 或默认本地地址
- `tls.enabled` 不填时会根据 `certFile`/`keyFile` 自动判断

## store

```yaml
store:
  driver: "postgres" | "memory"
  dsn: "postgres://user:pass@host:5432/account?sslmode=disable"
  maxOpenConns: 30
  maxIdleConns: 10
```

说明：
- `memory` 适合本地快速测试
- `postgres` 需要初始化 `sql/schema.sql`

## session

```yaml
session:
  ttl: 24h
```

注意：配置示例中出现的 `session.cache` / `session.redis` 字段在当前代码中未被读取。

## auth（JWT 令牌服务）

```yaml
auth:
  enable: true
  token:
    publicToken: "..."
    refreshSecret: "..."
    accessSecret: "..."
    accessExpiry: 1h
    refreshExpiry: 168h
```

说明：启用后会为 `/api/auth/*` 的保护路由添加 JWT 中间件。

### Root / RBAC 约束

- 系统仅允许一个 root 账号，固定邮箱：`admin@svc.plus`。
- 服务启动会自动执行 root 自检：
  - 若缺失 root 且未设置 `ROOT_BOOTSTRAP_PASSWORD`，服务启动失败；
  - 若存在旧版 `admin` 角色账号，会自动降级为 `operator`。
- 首次引导 root 账号时可设置环境变量：

```bash
ROOT_BOOTSTRAP_PASSWORD='scrubbed-now'
```

- `Demo` 体验账号固定为只读分组 `ReadOnly Role`，并使用 `readonly` 角色。

## smtp

```yaml
smtp:
  host: "smtp.example.com"
  port: 587
  username: "apikey"
  p: "s"
  from: "XWorkmate <no-reply@example.com>"
  replyTo: ""
  timeout: 10s
  tls:
    mode: "auto" | "starttls" | "implicit" | "none"
    insecureSkipVerify: false
```

说明：
- 未配置 `host` 或使用 `*.example.com` 时，邮件验证会自动关闭

## xray

```yaml
xray:
  sync:
    enabled: false
    interval: 5m
    outputPath: "/usr/local/etc/xray/config.json"
    templatePath: "account/config/xray.config.template.json"
    validateCommand: []
    restartCommand:
      - "systemctl"
      - "restart"
      - "xray.service"
```

## agent

```yaml
agent:
  id: "edge-node-1"
  controllerUrl: "https://accounts.svc.plus"
  apiToken: "replace-with-agent-token"
  httpTimeout: 15s
  statusInterval: 1m
  syncInterval: 5m
  tls:
    insecureSkipVerify: false
```

## agents（Controller 侧配置）

```yaml
agents:
  credentials:
    - id: "account-primary"
      name: "Account Server"
      token: "replace-with-agent-token"
      groups: ["default"]
```

该配置用于 Controller 校验 Agent 请求。
