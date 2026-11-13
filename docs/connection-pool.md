# 连接池与优雅关闭

阶段 4 在单连接多路复用的基础上增加每服务实例有界连接池、空闲连接回收、断连恢复和服务端 GracefulStop。

## 连接池策略

客户端默认配置：

| 配置 | 默认值 | 行为 |
|---|---:|---|
| `MaxConnections` | 4 | 单个服务地址最多保留 4 个活动连接 |
| `MaxIdleConnections` | 2 | 请求完成后最多保留 2 个空闲连接 |
| `MaxConcurrentCalls` | 1024 | 每个连接最多承载 1024 个未完成请求 |
| `IdleTimeout` | 5m | 空闲连接超过该时间后回收 |
| `DialTimeout` | 10s | 建连与断连重拨的默认超时 |
| `CallTimeout` | 10s | 未携带 deadline 的调用使用的默认总超时 |

连接按需创建。池中已有空闲连接时，新请求直接复用；所有连接达到单连接并发上限且连接数仍低于
`MaxConnections` 时，池会建立新连接。达到连接上限后，调用方等待已有连接释放并发槽。等待受
Context 约束，超时后返回 `RESOURCE_EXHAUSTED`，不会无限创建连接或 goroutine。

空闲回收采用两种触发方式：

- 请求释放时立即压缩空闲连接，确保空闲连接数不超过 `MaxIdleConnections`。
- 配置非零 `IdleTimeout` 时，后台回收器按超时关闭长期空闲连接。

## 连接选择与恢复

`internal/client.Client.Stats` 提供以下测试和运行指标：

- `Connections`、`IdleConnections`、`ActiveRequests`。
- `ConnectionsDialed`、`ConnectionsClosed`、`DialFailures`。
- `PoolWaits`、`PoolWaitFailures`。
- `LastCloseReason`，例如 `idle_limit`、`idle_timeout`、`transport_failure`、`client_closed`。

连接因 EOF、协议错误或网络关闭进入终态后，下一次请求会移除失效连接并按需重新拨号。其他活动请求
仍可安全返回错误；最后一个请求释放后，失效连接从池中删除。池不替换正在等待响应的 RequestID，
也不重放已经写入网络的请求，避免重复执行。

## 并发上限

`internal/transport.Conn` 为每个 TCP 连接维护 1024 个并发槽。超过上限的调用等待槽位、Context
结束或连接关闭。该限制同时保护 pending map、服务端分发 goroutine 和内存增长。

## GracefulStop

`server.Server.GracefulStop(ctx)` 的状态迁移：

1. 标记 server 为 draining，立即关闭 listener，不再接受新连接。
2. 已接受且在途的请求继续执行并写回响应。
3. draining 期间从既有连接读到的新请求返回 `UNAVAILABLE`。
4. 在途请求清空后关闭剩余连接并等待连接读取循环退出。
5. `ctx` 到期时标记 `graceful_timeout`、关闭连接并立即返回 `context.DeadlineExceeded`。

`server.Server.Stats` 暴露接受连接数、活动连接数、活动请求数、完成请求数、拒绝请求数、连接/并发/请求体拒绝次数、限流快照和关闭原因。默认连接与 Handler 并发上限均为 1024。
`Close` 仍保留为立即关闭语义，用于测试或进程被迫退出。

## 验收映射

| 验收项 | 测试 |
|---|---|
| 1000 个请求不创建 1000 个连接 | `TestPoolReusesConnectionsForThousandCalls` |
| 连接池达到上限时行为可预测 | `TestPoolCapacityFailureIsPredictable` |
| 空闲连接可以回收 | `TestPoolReclaimsIdleConnections` |
| 连接异常后后续请求可以恢复 | `TestPoolRecoversAfterConnectionLoss` |
| GracefulStop 保留已开始请求 | `TestGracefulStopWaitsForAcceptedRequest` |
| 停止流程有超时且不会永久阻塞 | `TestGracefulStopHonorsDeadline` |
| 单连接并发上限可被 Context 中止 | `TestConcurrencyLimitWaitsForAvailableSlot` |

并发路径由 `go test -race ./...` 覆盖。

## 当前限制

- 连接池绑定单个地址；服务发现和实例选择在后续阶段接入。
- 池不会对已写入但响应丢失的请求自动重放，重试属于后续治理阶段。
- 当前指标是进程内结构快照，尚未导出 Prometheus 格式。
