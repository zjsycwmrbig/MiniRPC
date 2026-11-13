# 阶段 3 报告：TCP Transport 与单机调用

## 阶段

阶段 3：实现 TCP Transport 和单机调用

## 主提交

- `cf3bf0091502dda07c11e9273119797d0ec5cc6e feat(transport): add multiplexed TCP unary RPC`

本报告使用独立文档提交记录上述实现基线，便于后续批次追踪和问题定位。

## 实现

- 实现 `internal/transport.Conn`，在单个 TCP 流上多路复用多个并发请求。
- 使用非零单调递增 `RequestID` 和 pending map 将响应准确关联到调用方。
- 对帧写入加互斥锁，保证同一连接上的帧字节不会交错。
- 在 Context 取消、写入失败和连接关闭时清理 pending 请求，避免等待者永久阻塞。
- 实现 TCP 客户端 `Dial`、`Invoke`、Protobuf 请求/响应编解码和远端错误解析。
- 在请求 Metadata 中透传 service、method、content type 和 deadline。
- 实现服务端监听、连接读取循环、服务注册校验、每请求并发分发和按连接串行写回。
- 使用生成器产出的 `NewRequest` 构造正确类型的 Protobuf 输入消息。
- 将 Handler panic 收敛为 `INTERNAL` 错误响应，避免单个请求终止服务进程。
- 定义稳定 RPC 错误码，并支持客户端通过 `errors.As` 获取 `*rpc.Error`。
- 增加可运行的 Echo Server 和 Echo Client，演示生成客户端到真实服务端的完整调用。
- 扩展代码生成器和生成代码，为每个 `MethodDesc` 增加请求构造函数。
- 增加 `docs/transport.md`，记录调用链、Metadata、错误码、并发语义和当前限制。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make fmt` | 通过 |
| `make proto` | 通过，重复生成后生成文件 SHA-256 不变 |
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过，全部包在竞态检测下运行成功 |
| `golangci-lint run` | 通过 |
| Echo Server 与 Echo Client 进程级调用 | 通过，客户端输出 `hello from batch 4` |

测试覆盖的关键行为：

- 响应通过 RequestID 命中正确等待者。
- 64 个 transport 层并发调用和 100 个真实 TCP Echo 并发调用共享一个连接。
- 客户端取消后 pending map 被清理，迟到的旧响应不会污染下一次调用。
- Client 或 Server 关闭时，未完成调用会被唤醒并返回错误。
- 不存在的 service 或 method 返回 `NOT_FOUND`。
- Handler 返回的业务错误码和消息可以跨进程还原。
- Handler panic 被转换为 `INTERNAL`。
- 生成客户端可以完成真实 loopback TCP Protobuf RPC。

## 验收

- [x] TCP Listener、Dialer 和单连接多路复用已经实现。
- [x] RequestID 分配、pending map 和并发响应关联测试通过。
- [x] 连接写入串行化，服务端业务 Handler 可以并发执行。
- [x] 生成客户端能完成真实 Echo 一元 RPC 调用。
- [x] deadline Metadata 可以转换为服务端请求 Context。
- [x] RPC 错误码、错误响应和 panic 隔离已经实现。
- [x] 全部测试通过竞态检测和静态检查。
- [x] 阶段实现和验收报告使用独立提交。
- [ ] GitHub Actions 远端运行：待仓库推送后确认。

## 已知限制

- 当前 Client 只管理一个 TCP 连接，尚无每实例连接池。
- 连接异常后不会自动重连，也尚未实现多实例切换。
- `Server.Close` 目前是立即关闭，不是完整的 GracefulStop。
- 客户端取消只清理本地 pending 状态，尚未发送 CANCEL 帧通知服务端。
- 单连接 1024 并发请求上限尚未实施。
- 尚未接入 etcd 注册发现和负载均衡。

## 下一阶段依赖

阶段 4 需要继续使用本阶段冻结的传输接口：

- 增加每实例连接池和连接复用。
- 限制单连接并发请求数，避免无界 goroutine 和 pending 增长。
- 实现可观测的优雅关闭流程，确保 listener、连接和在途请求的关闭顺序明确。
- 保持调用的 RequestID、pending 清理和错误语义不变。
