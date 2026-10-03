# Zero 签发 One peer 数据面配置

使用 `XCONNECT_OVERLAY_MESH_NETWORKS` 显式启用网络（逗号分隔 network UUID / ID），默认关闭。启用前部署兼容版本的 Gateway 与桌面 One。Zero 保留授权控制面职责：成员、公钥、overlay 单地址、双向 grant、generation、签名和 TTL；不选择 LAN 路径，也不通过遥测修改授权。

One 签名 v2 配置增加可选 `mesh.peers`；Gateway 签名配置增加可选 `mesh.peers[].allowed_peers`。省略字段时旧签名 bytes 保持不变。只发布 active 的桌面 linux / darwin / windows One，跳过移动端。peer 地址须 canonical 单地址 /32 或 /128、公钥 canonical，单节点最多 256 peers。

仅整设备双向授权允许 peer 直连 / peer relay：对应完整规则必须显式标记 `whole_device: true`，包含匹配 source / destination 的 ICMP、全部 TCP 和 UDP 端口，且不能存在匹配 deny。该标记明确授予所有 IP 协议的设备互通；普通 TCP/UDP 全端口加 ICMP 的规则也不自动提升为整设备授权。端口受限或单向授权仍走原 Gateway 路径；签发不扩大访问权限。既有 policy 严格默认 deny。

成员加入、撤销推进网络 generation，并在同一事务保留原规则、更新 policy revision，避免成员列表刷新遇到 revision 冲突。Gateway 和 One 自行验证更新及过期；断开控制面的节点在旧配置 TTL 前可能仍拥有旧 grant。

xconnect-edge-agent 继续负责 Gateway / One 注册上报，以及未来 GPG 证书、UUID、用户 Auth 的统一管理。新增的可选 overlay 报告由既有 agent status 接口接收，包含只读能力、路径、RTT、时间与健康字段；不接收数据面密钥，不形成另一套身份权威。

验证入口：`go test -race ./internal/overlay ./internal/agentserver ./api`，以及 Gateway 的跨仓库 `tests/mesh/run.sh`。本地和 PR 检查不代表 UAT 部署成功。
