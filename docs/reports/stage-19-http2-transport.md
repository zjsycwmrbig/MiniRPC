# 阶段 19 报告：HTTP/2 一元传输

## 结论

阶段 19 在既有 Mini-RPC v1 帧协议之上增加 HTTP/2 一元传输。一个 HTTP/2 stream 对应一个 Request/Response 帧，HTTP/2 负责连接内多路复用和流级取消，Mini-RPC 继续负责 Protobuf 方法分发、RequestID、Metadata、deadline、业务错误码、Trace 和治理语义。

本阶段保持 v1 帧字节布局、`pkg/rpc` 生成调用边界、现有 TCP 默认路径和稳定标签 `v0.1.0`（`ffdd515`）不变。HTTP/2 只在显式 `-http2`、`client.WithHTTP2()` 或 `client.DialHTTP2()` 时启用；流式 RPC、gRPC 互操作和浏览器 HTTP/1 fallback 尚未实现。

## 提交

- `445c5e7` `feat(transport): add HTTP/2 unary frame adapter`
- `2c5beed` `feat(server): dispatch unary RPC over HTTP/2 streams`
- `572ab55` `feat(client): pool governance-aware HTTP/2 connections`
- `69dbd42` `feat(cli): select HTTP/2 unary transport`
- `cfd9859` `fix(transport): normalize HTTP/2 client shutdown`
- `bda996e` `build(docker): verify HTTP/2 discovery RPC`
- `0de09d0` `ci(transport): gate HTTP/2 discovery RPC`
- `91d556c` `fix(server): gracefully drain HTTP/2 streams`
- `0186a71` `fix(transport): enforce HTTP/2 ALPN negotiation`
- `bd267fe` `docs(transport): document HTTP/2 unary transport`
- `9b460d0` `docs(reports): record stage 19 HTTP/2 transport`
- `4a9b1ec` `fix(ci): clear HTTP/2 lint findings`

## 实现范围

### 帧到 stream 映射

| 项目 | 约定 |
|---|---|
| Endpoint | `POST /mini-rpc.v1/Invoke` |
| Content-Type | `application/x-mini-rpc-frame` |
| Request Body | 恰好一个 Mini-RPC v1 Request 帧 |
| Success Body | 恰好一个 Response 帧 |
| 流取消 | 取消 HTTP/2 stream，并传播到服务端 Handler Context |
| RequestID | 客户端按连接分配，服务端响应复用；客户端校验响应一致 |

HTTP/2 adapter 会拒绝多个帧、尾随数据、错误帧类型、非 200 状态、错误 Content-Type 和 RequestID 不匹配。服务端读取上限为 v1 固定头部、最大 Header 和最大 Body 之和。

### 服务端

- `internal/server.Server.ServeHTTP2` 接受 h2c prior knowledge 连接；TLS 服务端自动加入 ALPN `h2`。
- `http2.Server.MaxConcurrentStreams` 与 `MaxConcurrentRequests` 对齐。
- 每个 stream 进入既有 `dispatchWithContext`，复用服务注册、保护性准入、令牌桶、Trace、指标和错误帧。
- GracefulStop 使用每次 HTTP/2 连接的 `http.Server.Shutdown` 停止新流并排空活动 Handler；关闭超时后沿既有强制关闭路径终止连接。
- 服务端 `Server.Close` 关闭活动连接并唤醒客户端 pending 调用。

### 客户端

- `internal/client.Transport` 增加 `TransportHTTP2`，并提供 `WithHTTP2` 和 `DialHTTP2`。
- HTTP/2 连接对象实现既有 transport `Call` 边界，因此 Resolver、有界连接池、负载均衡、重试、熔断、限流和 OTel 调用链复用。
- 一个 `HTTP2Conn` 可并发承载多个流；测试确认 100 个并发生成 RPC 共用一个 HTTP/2 connection。
- `Close` 统一关闭 `http2.ClientConn` 与底层网络连接，不会重复关闭或把正常 EOF 错报为协议错误。
- TLS 配置自动加入 `NextProtos = ["h2"]`；握手完成后仍要求 `NegotiatedProtocol == "h2"`，否则关闭连接并返回诊断错误。

### CLI、部署与 CI

- Echo Server/Client 增加 `-http2`。
- `deploy/docker-compose.h2c.yml` 启动 etcd、两个 h2c Echo Server、Prometheus 和一次性 Echo Client。
- `scripts/docker-http2-smoke.sh` 验证 etcd 发现、生成 Echo RPC、HTTP/2 服务端模式和指标，并在退出时清理 Compose 资源。
- `make docker-http2-test` 提供本地统一入口，CI Docker smoke job 通过该目标执行真实容器验收。

## 并发与关闭时序

~~~text
生成 Client.Invoke
  -> 从连接池获取 HTTP2Conn
  -> 编码单个 v1 Request 帧
  -> 分配 v1 RequestID
  -> POST /mini-rpc.v1/Invoke（独立 HTTP/2 stream）
  -> 服务端读取单帧并进入既有 dispatch
  -> Handler Context 绑定 stream cancellation/deadline
  -> 编码 v1 Response 帧
  -> 客户端校验 HTTP 状态、Content-Type、单帧边界和 RequestID
  -> 写入原 pending/治理结果路径
~~~

Context 取消后，HTTP/2 客户端取消对应 stream；服务端 Handler Context 同步取消，客户端返回 `CodeCanceled`，pending 计数归零。GracefulStop 在活动调用完成前不会提前返回。

## 安全边界

- TLS 最低版本仍为 1.2；mTLS、Root CA、ServerName、文件轮询和 last-known-good 热轮换沿用既有实现。
- 明文 HTTP/2 仅使用 prior-knowledge h2c，不提供 HTTP/1.1 Upgrade fallback。
- TLS ALPN 不是 `h2` 时连接立即关闭，避免把 HTTP/1.1 误交给 HTTP/2 adapter。
- HTTP/2 Header 限制为 64 KiB，Body 长度、Handler 并发、连接数和流数仍受原服务端资源策略限制。
- 日志、指标和报告不记录证书、私钥、认证 Header 或 OTLP 凭据。

## 本地验证

环境：Docker `golang:1.23-bookworm`，Docker Compose v2。

| 检查 | 结果 |
|---|---|
| `go test -race ./internal/transport ./internal/client ./internal/server` | 通过 |
| `go test -race ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go build -buildvcs=false ./...` | 通过 |
| `golangci-lint run`（v1.64.8） | 通过 |
| `make docker-http2-test` | 通过 |

容器验收最终输出：

~~~text
Docker HTTP/2 smoke test passed: h2c discovery, generated RPC, and metrics are healthy.
~~~

阶段测试覆盖：

- h2c 与 TLS ALPN 真实生成客户端调用。
- 100 个并发 HTTP/2 stream 复用单个连接。
- 请求取消传播到服务端 Context。
- 服务端 GracefulStop 排空活动调用，Server Close 唤醒 pending 调用。
- 单帧、Content-Type、尾随数据和 RequestID 响应校验。
- 非 `h2` ALPN 连接拒绝。
- etcd 发现、双 Echo Server、h2c RPC 和 Prometheus 指标容器链路。

## 兼容性与验收清单

- [x] Mini-RPC v1 帧字节布局保持不变。
- [x] 生成客户端与服务端接口保持不变。
- [x] TCP 仍是默认 transport，既有调用路径不受影响。
- [x] h2c prior knowledge 与 TLS ALPN `h2` 均可完成生成 Echo RPC。
- [x] 同一 HTTP/2 connection 支持 100 个并发一元流。
- [x] HTTP/2 stream cancellation 传播到 Handler Context。
- [x] 服务端 GracefulStop 等待活动流，Close 唤醒 pending 调用。
- [x] Resolver、连接池、治理、Trace 和指标继续进入同一调用链。
- [x] Docker 发现 RPC、双实例 h2c 服务端和指标验收通过。
- [x] 临时容器、网络、卷和本地镜像在 smoke test 后清理。
- [x] [GitHub Actions CI #43547939731](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43547939731) 最终验收通过：Verify、Lint 和完整 Docker smoke job 全部成功。

## 已知限制

- HTTP/2 当前仅支持一元 RPC；流式请求、流式响应和双向流未实现。
- 不兼容 gRPC wire protocol；Endpoint、Content-Type 和 Body 都是 Mini-RPC 自有约定。
- 仅支持直接 h2c prior knowledge，不支持 HTTP/1.1 Upgrade 或浏览器 HTTP/2 fetch fallback。
- HTTP/2 直接复用一个连接上的 stream，但没有新增跨连接 HTTP/2 专用调度、优先级或 Push 策略。
- 本阶段未提供 HTTP/2 benchmark；后续阶段 20 已补充独立 client/server OS 进程的 h2c、TLS 与 mTLS 性能验收。

## 下一阶段依赖

HTTP/2 一元传输已复用既有传输、连接池和治理边界。阶段 20 在该边界上提供独立进程性能证据和外部 target 模式；[阶段 20 报告](stage-20-http2-performance.md)记录 h2c、TLS、mTLS 的吞吐、尾延迟、建连分位和服务端资源。后续若引入流式 RPC，需要先单独定义 Stream ID、消息序列、半关闭、背压、错误传播和生成代码接口；若引入 gRPC 互操作，则需要独立适配 HTTP/2 trailer、gRPC status 和 message framing，不能改变现有 v1 帧语义。
