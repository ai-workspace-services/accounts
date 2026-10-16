# Agent 显式资源类型

节点通过 `X-Agent-Resource-Class` 传递 `AGENT_RESOURCE_CLASS`，Pool 名称只作为资源池标识。属性更新必须经过认证；请求头本身不授予管理权限。

| 类型 | 用户下发范围 |
| --- | --- |
| shared | Free + Plus，排除团队用户及黑名单 |
| free | 注册 Free 用户，包含未分组个人用户，排除黑名单 |
| plus | 有效 Plus 用户，排除黑名单 |
| team | 指定团队管理员和成员，排除其他团队、公共用户及黑名单 |

现有停用、邮箱验证、有效期及额度控制继续生效。黑名单以 `ListProxyBlockedAccountUUIDs` 为准，包括操作员暂停和计费停用；Sandbox 也不能绕过黑名单。

## 节点属性授权

- 配置凭据含 `agent:configure-public` 时，可为该凭据绑定的公共节点选择 shared/free/plus。
- 内部通配部署凭据（ID 为 `*`，含 `internal`，授权为 shared）可管理公共节点别名，兼容已有部署。旧数据库资源类型不会覆盖已授权的显式选择。
- 固定为 free/plus 的受限凭据，不能通过请求头或其他公共别名扩大范围。
- Team 仍由平台登记的团队权限确定；公共凭据不能切为 Team、覆盖团队节点，团队凭据不能切公共池或其他团队。
- 没有显式类型的旧请求沿用原有登记。建议部署时明确设置类型。

用户列表、SSE 通知及状态心跳使用相同的授权范围。状态心跳将生效类型写入节点库存及 PostgreSQL，套餐和成员数据不由 Agent 修改。revision 同时包含用户集合和 scope。

## 上线顺序

先发布 Accounts，再更新 Agent。Agent 保持对控制面 scope 的严格验证，错误日志显示期望及返回类型。只更新 Agent 无法纠正旧服务端忽略显式类型的问题。修改本地节点类型后，需要持有符合上面规则的部署凭据；禁止通过省略 scope 或删除校验绕过团队隔离。

## User-facing node discovery

The authenticated node/subscription endpoints use the same resource-class
boundary as agent UUID delivery. Free users see Free and Shared pools; Plus
users see Plus and Shared pools. Dedicated team administrators and members
see only their own team's pool. The existing account, blacklist, quota and
team-validity checks still run before discovery. Pool names do not select
resource class, including misleading names on explicit Shared registrations.
