# 阶段 5 报告：服务注册与发现

## 阶段

阶段 5：实现 etcd Registry、Lease KeepAlive、Watch、本地实例快照、Watch 重连和实例下线。

## 主提交

- `0006648 feat(registry): add etcd discovery and resilient snapshots`

本报告使用独立文档提交记录上述实现基线，便于后续批次追踪和问题定位。

## 实现

- 定义 `Instance`、`Event`、`Lease` 和 `Registry` 接口。
- 实例 key 使用 service 与 instance ID 的 Raw URL Base64 编码，避免分隔符注入。
- 基于 etcd client v3 实现 `Register`、`Deregister`、`List`、`Watch` 和 `Close`。
- 注册项绑定 etcd Lease，默认自动执行 KeepAlive，并可通过 `Lease.Revoke` 主动下线。
- Watch 携带 PrevKV，DELETE 事件可以从上一版记录还原完整实例。
- Watch 传输错误通过事件中的 `Err` 返回，调用方可以进行全量刷新。
- 实现客户端本地 `Snapshot`，首次读取后不再为每个调用同步访问 etcd。
- Resolver 将 PUT/DELETE 应用到本地实例列表，按实例 ID 稳定排序，并复制 Metadata。
- Watch 断开后按指数退避重连，每次重连前执行全量 List。
- List 暂时失败时保留最后一份可用实例快照，同时通过 `Snapshot.Err` 暴露故障。
- Echo Server 增加 etcd endpoint、instance ID、advertise address 和 lease TTL 参数。
- Echo Server 关闭时先 Revoke，再 GracefulStop，避免新流量继续发现下线实例。
- 增加 Dockerfile 和 Compose 环境，可一键启动 etcd 与两个已注册 Echo Server。
- 新增 `docs/registry.md`，记录 key、Lease、Watch 和快照语义。

## 测试

使用 Go 1.23.12、protoc 28.3、golangci-lint v1.64.8、etcd v3.5.17 和 Docker 26.0.1 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过 |
| `go test ./...` | 通过 |
| `go test -race ./...` | 通过 |
| `go test -race -count=3 ./internal/registry ./internal/resolver` | 通过 |
| `golangci-lint run` | 通过 |
| `go test -tags=integration ./internal/registry -run TestEtcdRegistryLifecycle` | 通过，真实 etcd 2.6 秒生命周期测试 |
| `go test -race -tags=integration ./internal/registry -run TestEtcdRegistryLifecycle` | 通过 |
| `docker compose -f deploy/docker-compose.yml up -d --build` | 通过，etcd healthy，两个 Echo Server 正常运行 |
| Compose 内 Echo RPC | 通过，客户端输出 `stage5-compose` |
| 停止并重新启动 `echo-b` | 通过，key 先消失后重新注册 |
| GitHub Actions `Verify` 与 `Lint` | 通过 |

测试覆盖的关键行为：

- 一个和两个实例注册后均可被 List 发现。
- Watch 能按顺序收到 PUT 与 DELETE，并从 PrevKV 还原删除实例。
- TTL 到期前 KeepAlive 维持实例；Revoke 后立即从 List 和 Watch 消失。
- 两个 Compose Echo 实例均出现在 `/mini-rpc/registry` 前缀下。
- 停止 `echo-b` 后 key 消失，重新启动后 key 恢复。
- Resolver 首次 Resolve 只执行一次 List，后续调用读取本地快照。
- PUT/DELETE 事件更新本地实例列表。
- Watch 关闭后 Resolver 全量刷新并建立新 Watch。
- List 失败时保留上一份实例快照并记录错误。

## 验收

- [x] Registry 接口与 etcd 实现完成。
- [x] Lease 注册、自动 KeepAlive 与主动 Revoke 完成。
- [x] List 与 Watch 完成，删除事件保留实例内容。
- [x] Resolver 维护本地快照，不在每次请求中同步访问 etcd。
- [x] Watch 断开后执行退避重连与全量刷新。
- [x] 注册中心不可用时保留已有快照。
- [x] 实例停止后从注册中心移除，恢复后重新出现。
- [x] Compose 可启动 etcd 与两个 Echo Server。
- [x] 真实 etcd 集成测试可在独立标签下执行。
- [x] 阶段实现和验收报告使用独立提交。
- [x] GitHub Actions `Verify` 与 `Lint` 在提交 `feabcde` 上通过。

## 已知限制

- 实例选择和负载均衡将在阶段 6 接入 Resolver 快照。
- Watch 重连由每个 Resolver 独立处理，尚未建立跨服务的 Watch 连接池。
- etcd TLS、鉴权、多 endpoint 故障域和压缩恢复策略留到后续工程化阶段。
- 指标目前是进程内结构快照，尚未导出 Prometheus。

## 下一阶段依赖

阶段 6 负载均衡需要消费本阶段冻结的 Resolver 快照：

- 在稳定实例快照上实现 Round Robin 和 Weighted Round Robin。
- 并发 Pick 不修改快照。
- 实例下线或权重变化后停止选择旧实例。
- 空实例列表和并发更新具有明确错误语义。
