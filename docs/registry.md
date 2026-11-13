# 服务注册与发现

阶段 5 引入 etcd Registry、Lease KeepAlive、Watch 和客户端本地实例快照。

## 实例模型

```go
type Instance struct {
    ID       string
    Service  string
    Address  string
    Weight   int
    Metadata map[string]string
}
```

服务名和实例 ID 共同确定唯一注册项。实例值以 JSON 存储在 etcd：

```text
/mini-rpc/registry/<base64(service)>/<base64(instance-id)>
```

Service 与 ID 使用 Raw URL Base64 编码，避免分隔符注入并保持 key 可预测。

## Lease 与 KeepAlive

`EtcdRegistry.Register` 执行以下步骤：

1. 校验实例、TTL 和 Registry 状态。
2. 向 etcd 申请 Lease。
3. 使用 `WithLease` 写入实例 key。
4. 启动后台 KeepAlive。
5. 返回 `registry.Lease`，其 `Done` 通道在续租停止时关闭。

`Lease.Revoke` 主动删除注册项并停止续租。进程异常退出时不会执行 Revoke，实例在 TTL
到期后自动消失。Echo Server 收到 SIGINT/SIGTERM 时先 Revoke，再执行 GracefulStop。

## Watch

`EtcdRegistry.Watch` 使用服务前缀和 `WithPrevKV`：

- PUT 转换为 `EventPut`。
- DELETE 从 `PrevKv` 恢复实例信息并转换为 `EventDelete`。
- Watch 失败时先发送带 `Err` 的事件，再关闭输出 channel。

## Resolver 快照

`internal/resolver.Resolver` 维护每个服务的不可变快照：

```go
type Snapshot struct {
    Service   string
    Instances []registry.Instance
    UpdatedAt time.Time
    Err       error
}
```

- 首次 `Resolve` 执行一次 `List`，后续请求只读取本地快照。
- `Watch` 先建立 Watch，再执行全量 `List`，降低漏掉注册变化的窗口。
- Watch 断开、输出关闭或收到错误事件后，Resolver 按指数退避重连，并在每次重连前重新全量
  `List`。
- 注册中心暂时不可用时，上一份实例列表继续可用；失败原因写入 `Snapshot.Err`。
- 快照按实例 ID 排序，Metadata 在返回前复制，调用方不能修改 Resolver 内部状态。

## 运行和集成测试

启动 etcd 与两个已注册的 Echo Server：

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml ps
docker compose -f deploy/docker-compose.yml exec etcd \
  etcdctl --endpoints=http://127.0.0.1:2379 get /mini-rpc/registry --prefix
```

真实 etcd 测试使用构建标签，不进入默认单元测试：

```bash
ETCD_ENDPOINT=127.0.0.1:2379 \
  go test -tags=integration ./internal/registry -run TestEtcdRegistryLifecycle -v
```

默认 `go test ./...` 使用内存 Fake Registry 验证 Resolver 快照、PUT/DELETE 和 Watch
重连，不依赖 etcd。

## 验证范围

- 一个和两个实例的注册、List 结果。
- Watch 的 PUT 和 DELETE 顺序与实例内容。
- TTL 到期前 KeepAlive 维持注册。
- Revoke 后实例立即从 List 和 watch 中消失。
- 两个实例启动后均出现在 etcd。
- 停止一个 Echo Server 后其 key 消失，重新启动后再次出现。
- Watch channel 关闭后 Resolver 全量刷新并恢复订阅。
- List 失败时保留上一份可用快照。

## 当前限制

- Registry 只提供实例存储和 Watch；实例选择与负载均衡在阶段 6 实现。
- Watch 当前由 Resolver 重连；Registry 自身不维护全局 Watch 连接池。
- 鉴权、TLS 和多 etcd endpoint 故障域策略留到工程化阶段完善。
- 当前 Compose 暴露 etcd 客户端端口，仅用于本地验证。
