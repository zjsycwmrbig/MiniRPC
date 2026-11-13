# Changelog

本项目的主要变更记录在此文件中。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本遵循语义化版本。

## [Unreleased]

### Added

- RPC 通道支持 TLS 1.2/1.3 服务端认证，客户端默认校验 CA 与实例服务器名称。
- 服务端 `-tls-client-ca` 支持双向 TLS，并要求每个客户端证书通过可信 CA 和 clientAuth 用途校验。
- Echo Server/Client 支持证书、私钥、CA、最低版本和握手超时参数；`internal/security` 负责严格的 PEM 加载与配置验证。
- Prometheus 增加 `rpc_tls_handshake_failures_total{role="server"}`。
- 新增一次性测试证书脚本、Docker mTLS Compose override、`make docker-tls-test` 和真实双实例发现 mTLS smoke test。

### Changed

- v0.2 扩展阶段保持 v1 帧协议与现有明文部署兼容；只有显式配置 TLS 时启用加密。

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
