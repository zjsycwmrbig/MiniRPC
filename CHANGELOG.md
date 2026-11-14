# Changelog

本项目的主要变更记录在此文件中。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本遵循语义化版本。

## [Unreleased]

### Added

- RPC 通道支持 TLS 1.2/1.3 服务端认证，客户端默认校验 CA 与实例服务器名称。
- 服务端 `-tls-client-ca` 支持双向 TLS，并要求每个客户端证书通过可信 CA 和 clientAuth 用途校验。
- Echo Server/Client 支持证书、私钥、CA、最低版本和握手超时参数；`internal/security` 负责严格的 PEM 加载与配置验证。
- Prometheus 增加 `rpc_tls_handshake_failures_total{role="server"}`。
- TLS 服务端证书、私钥、客户端 CA，以及客户端根 CA、证书和私钥支持文件轮询热加载；候选材料完整验证后才原子发布，失败时保留 last-known-good 配置。
- Echo Server/Client 增加 `-tls-reload-interval`，默认 `10s`，`0` 关闭轮询；新连接使用当前版本，既有连接保持原握手配置。
- Prometheus 增加有界的 TLS 热加载成功/失败计数和当前配置版本指标。
- 新增一次性测试证书脚本、Docker mTLS Compose override、`make docker-tls-test` 和真实双实例发现 mTLS smoke test。
- 新增 `make docker-tls-rotation-test`，在线验证完整 mTLS 材料轮换、退役客户端身份拒绝和无效更新回退。
- Benchmark 增加 `-transports=plaintext,tls,mtls`、`-handshakes`、真实 TLS/mTLS 并发场景、连接建立分位数据和 `make benchmark-smoke`。
- 新增 OpenTelemetry SDK 和 OTLP/HTTP Trace Exporter；客户端与服务端分别生成 Client/Server Span，服务端通过远程父 Span 继续同一 Trace。
- Span 记录 `rpc.system`、`rpc.service`、`rpc.method`、角色、attempt、outcome 和稳定错误码；Resource 记录 `service.name` 与可选版本、实例 ID。
- 新增 ParentBased + TraceIDRatioBased 根采样，并通过请求 Metadata 的 `trace_flags` 将采样决定传播到服务端。
- Echo Server/Client 新增 `-otlp-endpoint`、`-otlp-insecure`、`-otlp-headers`、`-otlp-sample-ratio` 和 `-otlp-export-timeout`；退出时 flush batch。
- 新增固定版本 OpenTelemetry Collector Compose override、OTLP debug exporter、`make docker-otel-test` 和 CI 门禁，验证客户端/服务端 Span 共享同一 Trace ID。
- OTLP/HTTP Exporter 支持自定义根 CA、客户端证书和 Collector mTLS；Echo Server/Client 新增 `-otlp-ca`、`-otlp-cert`、`-otlp-key`、`-otlp-tls-server-name` 和 `-otlp-tls-min-version`。
- 新增 Collector mTLS Compose override、带 `otel-collector` SAN 的一次性测试证书和 `make docker-otel-mtls-test`，验证私有 CA、客户端身份、服务名校验和 Trace 关联。

### Changed

- v0.2 扩展阶段保持 v1 帧协议与现有明文部署兼容；只有显式配置 TLS 时启用加密。
- TLS 材料更新按 New handshake / New dial 生效，不替换已经建立的连接，也不改变 RPC 帧或连接池语义。
- 配置 OTLP Exporter 后，日志中的 Trace/Span ID 和采样决定来自 OpenTelemetry TracerProvider；未配置 Exporter 时继续使用进程内标识生成，RPC 日志字段保持兼容。
- HTTPS OTLP Exporter 始终显式设置不低于 TLS 1.2 的最低版本；未提供自定义根 CA 时继续使用系统信任根。

### Fixed

- 修复对端在写完最终响应后立即 EOF 时，多路复用客户端可能以 `transport: connection closed` 覆盖已解码响应的问题；调用结束信号现在只由一次明确的响应或终止错误触发。

### Security

- OTLP Endpoint 只接受 `http`/`https` 绝对 URL；明文 HTTP 必须显式启用 insecure，HTTPS 不允许 insecure，URL 凭据和 fragment 会被拒绝。
- 静态 OTLP Header 在启动时严格校验，解析错误不回显 Header 值；URL、Header 和 Collector 凭据不得写入日志或 Metrics。
- OTLP TLS 配置只允许用于 HTTPS；拒绝 `InsecureSkipVerify` 和低于 TLS 1.2 的最低版本，Endpoint 未设置服务器名称时从主机名推导 SAN 校验目标。
- OTLP CA、客户端证书、私钥、Header 和 Collector 凭据不进入日志或 Metrics；仓库和 CI 只使用运行时生成、测试后删除的一次性材料。

### Known limitations

- 当前只实现 OTLP/HTTP Trace 导出，不包含 OTLP/gRPC、Baggage 或 Span Events。
- OTLP CA、客户端证书和私钥只在进程启动时加载，尚未纳入证书热加载；轮换后需要重启导出进程。
- OTLP mTLS 保护 Exporter 到 Collector 的传输身份，不替代静态 Header、Token 鉴权或 Collector 侧授权策略。
- 采样比例只作用于本地根 Span，服务端继承上游采样决定；需要全链路一致策略时，各入口必须配置兼容的采样率。

## [v0.1.0] - 2026-11-13

首个可本地运行、可注入故障并通过竞态检测的 Go RPC 框架基线版本。

### Added

- Protobuf Echo API、类型安全的客户端/服务端生成代码和固定版本代码生成工具链。
- 带 Magic、Version、Type、RequestID、Header 和 Body 长度校验的二进制帧协议。
- TCP 长连接、同连接并发请求关联、deadline 传播、请求取消和 pending 请求清理。
- 每地址有界连接池、空闲连接回收、断连恢复和客户端/服务端优雅关闭。
- etcd v3 服务注册、Lease、KeepAlive、List、Watch、Watch 重连和本地 Resolver 快照降级。
- Round Robin 与加权 Round Robin 实例选择，以及基于发现快照的跨实例故障切换。
- 显式幂等调用判定、有限重试、指数退避、抖动和 Closed/Open/HalfOpen 三态熔断。
- 客户端与服务端令牌桶限流，以及连接数、Handler 并发数和请求体大小保护。
- JSON 结构化日志、request_id/trace_id/span_id 关联、父子 Span 和 Prometheus 文本指标。
- Echo Server、Echo Client、Docker Compose、Prometheus 抓取配置和容器化 smoke test。
- 延迟、丢响应、响应替换、强制断连、实例停止/恢复、注册中心抖动和资源耗尽故障演练。
- 串行、单连接并发、连接池并发、双实例 Round Robin、加权 Round Robin 和治理开关 benchmark。
- 完整技术设计、专题文档、阶段验收报告、贡献指南和性能报告。

### Fixed

- GracefulStop 在最终响应写回前关闭连接的问题；现在先排空请求并半关闭 TCP，再等待连接 worker 退出。
- 极短 wall-clock deadline 在 race detector 调度延迟下过早取消的问题；生产 deadline 语义保持不变。
- 本地限流拒绝被发现层误判为跨实例故障并重试的问题。

### Security

- 服务端在读取和执行业务 Handler 前执行长度、连接、并发和限流边界检查。
- 自动跨实例重试只对显式标记幂等的调用启用，避免未确认写操作被隐式重放。
- Docker Compose 的宿主机端口默认只绑定 `127.0.0.1`。

### Known limitations

- 不支持流式 RPC、完整 HTTP/2、多语言生成、生产级认证授权或分布式事务。
- Resolver 与指标保存在进程内；控制面快照和指标历史不会跨重启持久化。
- 故障注入不覆盖真实网络分区、帧内损坏、乱序、重复帧或长时间随机混沌。
- Benchmark 在单一 Linux 主机、同一进程内运行客户端和本地 Server，不代表独立生产节点的容量。
- Docker 镜像沿用可读版本标签，尚未固定到镜像 digest，也未运行容器用户权限加固。

[v0.1.0]: https://github.com/zjsycwmrbig/MiniRPC/releases/tag/v0.1.0
