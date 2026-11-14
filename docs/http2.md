# HTTP/2 一元传输

阶段 19 为 Mini-RPC 增加 HTTP/2 一元 RPC 传输。HTTP/2 负责连接上的流多路复用、流级取消和流量控制，Mini-RPC v1 帧继续作为每个流的应用层请求与响应信封。该实现保持既有 Protobuf 生成代码、Metadata、错误码和治理调用链不变。

## 范围与非目标

当前支持：

- 一个 HTTP/2 stream 承载一个 Mini-RPC v1 Request 帧，并返回一个 Response 帧。
- 明文 h2c prior knowledge。
- TLS 最低 1.2，并通过 ALPN 协商 `h2`。
- 同一连接上的并发一元调用、Context 取消和 deadline。
- 客户端有界连接池、Resolver、负载均衡、重试、熔断、限流、Trace 和指标复用原调用链。
- HTTP/2 服务端 GracefulStop、活动流排空、请求数/并发/请求体保护。

当前不支持：

- gRPC wire protocol 或 gRPC 客户端互操作；HTTP/2 Body 仍是 Mini-RPC v1 二进制帧。
- 客户端流、服务端流和双向流 RPC。
- HTTP/1.1 升级、浏览器 `fetch` fallback 或 HTTP/2 WebSocket 扩展。
- 跨连接 Push、动态 HTTP/2 配置控制面或独立传输层指标。

## 线路映射

| HTTP/2 属性 | Mini-RPC 约定 |
|---|---|
| Method | `POST` |
| Path | `/mini-rpc.v1/Invoke` |
| Content-Type | `application/x-mini-rpc-frame` |
| Accept | `application/x-mini-rpc-frame` |
| Body | 恰好一个完整 Mini-RPC v1 Request 帧 |
| 成功响应 | HTTP 200，Content-Type 相同，Body 恰好一个 Response 帧 |
| 取消 | Context 取消会取消对应 HTTP/2 stream 并传播到服务端 Handler Context |
| 错误 | RPC 业务错误仍通过带 Error flag 和 `error_code` 的 v1 Response 帧返回 |

请求固定头部、Metadata、Body、Magic、Version、RequestID 和长度上限没有变化，详见[二进制协议 v1](protocol.md)。

客户端会为每个流分配连接内唯一 RequestID，并校验响应 RequestID 与请求一致。服务端还会拒绝非 Request 帧、多个尾随帧、错误 Content-Type 和超过“固定头部 + 最大 Header + 最大 Body”的 HTTP Body。

## 连接建立

### h2c

未配置 TLS 时，Echo Client 和 Echo Server 使用 HTTP/2 prior knowledge，不发送 HTTP/1.1 Upgrade，也不尝试 HTTP/1.1 fallback。客户端与已有 TCP 连接池复用同一地址的 HTTP/2 ClientConn。

### TLS

配置 TLS 后，服务端在 TLS `NextProtos` 中加入 `h2`，客户端也会确保声明 `h2`。TLS 握手完成后，客户端必须确认 `ConnectionState().NegotiatedProtocol == "h2"`；否则关闭连接并返回错误，不会把 HTTP/1.1 TLS 连接交给 HTTP/2 adapter。

证书、根 CA、mTLS、服务器名称校验、最低 TLS 1.2 和材料热轮换继续使用 [RPC TLS 与双向 TLS](tls-mtls.md) 的实现。TLS 轮换按新握手生效，既有 HTTP/2 连接保持原 TLS 会话。

## 客户端 API

在已有选项上增加：

```go
clientConn, err := client.DialHTTP2(ctx, address, client.WithTLSConfig(tlsConfig))
```

也可以使用通用入口：

```go
clientConn, err := client.Dial(ctx, address, client.WithHTTP2())
```

生成的 Echo Client 不感知传输类型，仍通过稳定的 `pkg/rpc.ClientConn.Invoke` 调用。HTTP/2 adapter 在 Client 内部实现相同的 `Call` 边界，因此 Resolver 快照选择、连接池、治理和 Trace 不需要分叉。

## 服务端 API

h2c 服务端使用：

```go
err := rpcServer.ServeHTTP2(listener)
```

配置 TLS 后，同一方法仍然使用；服务端自动加入 ALPN。Echo Server 的 `-http2` 参数会在监听前选择该入口：

```bash
go run ./cmd/echo-server -addr=127.0.0.1:8080 -http2
go run ./cmd/echo-client -addr=127.0.0.1:8080 -http2
```

每个 HTTP/2 connection 使用独立的 `http.Server.Shutdown`。GracefulStop 停止接受新流并等待活动 Handler；超过关闭预算时，原有强制关闭路径取消剩余流并唤醒客户端调用。

## 并发与资源边界

- 一个 HTTP/2 连接可并发承载最多 `MaxConcurrentRequests` 个流；服务端也使用该值设置 HTTP/2 `MaxConcurrentStreams`。
- 客户端连接池、Server 连接数上限、Handler 并发上限、请求体上限、令牌桶和熔断状态继续生效。
- 客户端 pending 计数用于测试和关闭路径观察；响应完成后降低，取消或连接终止不会留下永久等待。
- HTTP/2 Header 上限沿用 64 KiB 约束；压缩后的 HTTP/2 帧仍按解压后 Body 与协议长度上限读取。
- 服务端只接受单个 v1 帧。额外字节或第二个帧返回 400 并计入拒绝请求，避免 HTTP/2 stream 边界与 RPC 帧边界不一致。

## 可观测性与错误

HTTP/2 调用继续进入原有结构日志、Prometheus 指标和 OpenTelemetry Span。服务端从 v1 Metadata 创建相同 Handler Context 和远程父 Span；HTTP/2 stream cancellation 映射为 `CodeCanceled`，deadline 映射保持原错误码。

HTTP 层非 200 状态、错误 Content-Type、响应 RequestID 不匹配或尾随帧属于传输协议错误，不伪装成业务 RPC 成功。业务 Handler 返回的错误仍以 v1 Error Response 帧传递。

## 测试与验收

```bash
go test -race ./internal/transport ./internal/client ./internal/server
make docker-http2-test
make benchmark-http2-smoke
```

测试覆盖：

- h2c 与 TLS ALPN 的真实生成客户端调用。
- 单连接 100 个并发流。
- HTTP/2 取消传播到服务端 Context。
- 服务端 GracefulStop 排空活动调用。
- Server Close 唤醒 pending 调用。
- 多帧、错误 Content-Type 和 RequestID 不匹配拒绝。
- 非 `h2` ALPN 连接被客户端 adapter 拒绝。
- Docker etcd 发现、双 Echo Server、h2c RPC 和指标 smoke test。
- 独立 benchmark 与 Echo Server 进程完成 h2c、HTTP/2 TLS、HTTP/2 mTLS 真实 RPC，并采集服务端 CPU/RSS。

## 已知限制

HTTP/2 当前只是一元 RPC 的替代传输，不改变 v1 帧或代码生成接口。只有显式选择 `-http2` 或 `WithHTTP2` 时才启用；默认仍为 TCP transport。独立进程性能验收仍运行在共享主机和回环网络，不能外推为独立生产节点的容量承诺。HTTP/2 与 gRPC、流式 RPC、浏览器 fallback 的兼容性需要后续独立设计。

阶段 19 的传输实现与功能验收见[阶段 19 报告](reports/stage-19-http2-transport.md)，独立进程吞吐、尾延迟、建连成本和资源证据见[阶段 20 报告](reports/stage-20-http2-performance.md)。
