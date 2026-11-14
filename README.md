# Mini-RPC

Mini-RPC 是一个面向 Go 微服务场景的可治理 RPC 框架项目。v0.1.0 覆盖 Protobuf 接口与代码生成、TCP 长连接与一元 RPC、服务注册发现、负载均衡、超时取消、重试熔断、令牌桶限流、可观测性、优雅关闭，以及集成测试、故障注入和性能基准；v0.2 扩展继续加入 RPC TLS、mTLS、加密传输性能验收、证书热加载、OpenTelemetry OTLP Trace 导出、带自定义根证书与安全材料轮换的遥测链路，以及保持 v1 帧字节布局不变的 HTTP/2 一元传输。

稳定基线为 **v0.1.0**，13 个实现批次已全部完成；**v0.2 开发线**已完成 TLS/mTLS 传输安全、TLS/mTLS benchmark、带失败回退和滚动更新的证书热加载、可关联客户端/服务端 Span 的 OTLP Trace 导出、Collector 自定义 CA/mTLS 接入、OTLP TLS 材料热轮换，以及 HTTP/2 一元 RPC 传输。仓库具备生成代码、二进制帧协议、TCP 或 HTTP/2 单连接并发调用、每实例有界连接池、etcd Lease/KeepAlive/Watch、本地实例快照、Watch 重连、Round Robin、加权 Round Robin、deadline 透传、CANCEL 与流取消、有限幂等重试、退避与熔断、令牌桶和资源保护、结构化日志、OpenTelemetry Trace、Prometheus 指标、GracefulStop、故障切换、明文/TLS/mTLS 性能基准、Docker Compose，以及证书验证、双向 TLS、last-known-good 热轮换和 h2c/ALPN 传输选择。

## 环境要求

- Go 1.23 或更高版本
- GNU Make
- Git
- Protocol Buffers compiler（`protoc` 28.3 或兼容版本）
- Docker 与 Compose v2（仅运行容器验收时需要）
- OpenSSL（运行 RPC TLS、证书轮换或 OTLP Collector mTLS Docker 验收时需要）

Go 代码生成器由 `make tools` 安装到被 Git 忽略的 `bin/` 目录：

- `protoc-gen-go` v1.36.6
- 项目内置的 `protoc-gen-minirpc`

## 快速开始

克隆或进入仓库后，先检查 Go module，生成 API 代码，再执行本地 CI 等价检查：

```bash
go version
go env GOMOD
make tools
make proto
make check
```

启动本地 Echo Server。可观测性默认开启，Prometheus 指标监听 `127.0.0.1:9090`：

```bash
go run ./cmd/echo-server \
  -addr 127.0.0.1:8080 \
  -metrics-addr 127.0.0.1:9090 \
  -instance-id echo-server-1
```

也可以启用服务端限流和资源上限：

```bash
go run ./cmd/echo-server \
  -addr 127.0.0.1:8080 \
  -rate 20 -burst 40 \
  -max-connections 256 \
  -max-concurrent-requests 128 \
  -max-request-body-bytes 1048576
```

在另一个终端调用：

```bash
go run ./cmd/echo-client -addr 127.0.0.1:8080 -message "hello mini-rpc"
```

客户端会输出服务端回显的消息，并向 stderr 写入一条 JSON 完成日志。抓取服务端指标：

```bash
curl -sS http://127.0.0.1:9090/metrics
```

连接 etcd 并使用服务发现时，Echo Client 会从 Resolver 快照选择实例，并在显式幂等调用中执行有界故障切换：

```bash
go run ./cmd/echo-client \
  -etcd-endpoints 127.0.0.1:2379 \
  -retry-attempts 3 \
  -message "hello through discovery"
```

客户端和服务端日志具有相同 `trace_id` 与 `request_id`，服务端 `parent_span_id` 指向客户端 `span_id`。需要开始事件时给任一端增加 `-debug`；设置 `-observability=false` 可以完全关闭标识生成、日志和指标。

Echo 示例通过 `rpc.WithIdempotent(ctx)` 标记只读调用。写操作只有在业务确认重复执行安全后才能使用相同标记参与自动重试：

```go
ctx = rpc.WithIdempotent(ctx)
response, err := echoClient.Echo(ctx, request)
```

如果修改了 Protobuf 定义，请先执行 `make proto`，提交更新后的生成文件，再运行 `make proto-check` 确认重新生成不会产生差异。

查看全部开发命令：

```bash
make help
```

常用命令：

| 命令 | 用途 |
|---|---|
| `make tools` | 安装固定版本的 Go 代码生成器 |
| `make proto` | 从 `.proto` 生成消息类型和 Mini-RPC 适配器 |
| `make proto-check` | 检查已提交的生成代码是否与 `.proto` 一致 |
| `make fmt` | 格式化全部 Go 文件 |
| `make fmt-check` | 检查格式，不修改文件 |
| `make vet` | 运行 `go vet` |
| `make test` | 运行单元测试 |
| `make race` | 开启竞态检测运行测试 |
| `make fault-test` | 运行全部确定性故障切换和恢复演练 |
| `make benchmark` | 运行明文、TLS、mTLS 真实 Echo 场景，输出吞吐、延迟分位、资源连接和建连成本 |
| `make benchmark-smoke` | 运行明文、TLS、mTLS 的短 benchmark 工具与真实 RPC 校验 |
| `make docker-test` | 构建 Compose 环境，执行发现 RPC 和指标 smoke test 后清理资源 |
| `make docker-http2-test` | 通过 etcd 发现验证 h2c 一元 RPC、单连接流复用和指标 |
| `make docker-tls-test` | 生成一次性证书，验证发现、RPC mTLS 和握手失败指标后清理资源 |
| `make docker-tls-rotation-test` | 在线轮换证书、私钥与 CA，验证新握手、旧信任拒绝和失败回退 |
| `make docker-otel-test` | 通过 OpenTelemetry Collector 验证客户端/服务端 OTLP Span 关联 |
| `make docker-otel-mtls-test` | 用私有 CA 和客户端证书验证 Collector mTLS、服务名校验与 Trace 关联 |
| `make docker-otel-mtls-rotation-test` | 在线轮换 Collector 与服务端/客户端的 OTLP CA、证书和私钥 |
| `make build` | 编译全部 Go 包 |
| `make check` | 运行生成一致性、格式、vet、测试和构建检查 |
| `make lint` | 运行 golangci-lint（需要本机安装） |

## 目录结构

```text
MiniRPC/
├── api/                  # Protobuf API 和生成代码
├── cmd/                  # Echo Server、Client 等可执行程序
│   ├── echo-client/      # Echo RPC 客户端
│   ├── echo-server/      # Echo RPC 服务端
│   └── protoc-gen-minirpc/ # Mini-RPC 客户端与服务端适配器生成器
├── internal/             # 协议、传输、治理、注册中心等内部实现
│   ├── balancer/         # 负载均衡策略
│   ├── client/           # 客户端调用与连接选择
│   ├── codec/            # 序列化编解码
│   ├── governance/       # 超时、重试、熔断和限流
│   ├── observability/    # 日志、指标、Trace 与 OTLP Exporter
│   ├── protocol/         # 二进制帧协议
│   ├── registry/         # etcd 等注册中心适配器
│   ├── resolver/         # 客户端实例快照
│   ├── server/           # 服务端分发与生命周期
│   ├── security/         # RPC TLS 证书加载、校验与原子热轮换
│   ├── testkit/          # 测试服务器、代理和故障注入工具
│   └── transport/        # TCP 连接与请求关联、HTTP/2 流适配
├── pkg/rpc/              # 稳定的对外 API
├── benchmark/            # 压测程序和报告
├── fault-injection/      # 故障注入场景
├── deploy/               # Docker Compose 与本地依赖
├── docs/                 # 设计、协议、运维和阶段报告
├── Makefile              # 统一开发入口
└── go.mod                # Go module
```

`pkg/rpc` 只暴露稳定用户接口，具体实现保留在 `internal/`。协议、治理、注册发现、可观测性和测试工具分别保持独立包边界。

## 文档

- [架构概览](docs/architecture.md)
- [代码生成](docs/code-generation.md)
- [二进制协议](docs/protocol.md)
- [TCP Transport](docs/transport.md)
- [HTTP/2 一元传输](docs/http2.md)
- [连接池与优雅关闭](docs/connection-pool.md)
- [服务注册与发现](docs/registry.md)
- [负载均衡](docs/load-balancing.md)
- [超时与请求取消](docs/timeout.md)
- [重试与熔断](docs/retry-and-circuit-breaker.md)
- [限流与资源保护](docs/rate-limiting-and-resource-protection.md)
- [可观测性](docs/observability.md)
- [故障注入与恢复验证](docs/failure-injection.md)
- [RPC TLS 与双向 TLS](docs/tls-mtls.md)
- [性能基准](docs/benchmarking.md)
- [阶段 12 性能与发布验收](docs/reports/stage-12-performance-release.md)
- [阶段 13 TLS 与 mTLS 验收](docs/reports/stage-13-tls-mtls.md)
- [阶段 14 TLS 与 mTLS 性能验收](docs/reports/stage-14-tls-performance.md)
- [阶段 15 TLS 证书热加载与轮换验收](docs/reports/stage-15-tls-certificate-rotation.md)
- [阶段 16 OpenTelemetry OTLP Trace 验收](docs/reports/stage-16-otlp-tracing.md)
- [阶段 17 OTLP Collector 自定义 CA 与 mTLS 验收](docs/reports/stage-17-otlp-collector-mtls.md)
- [阶段 18 OTLP TLS 材料热加载与轮换验收](docs/reports/stage-18-otlp-tls-rotation.md)
- [阶段 19 HTTP/2 一元传输验收](docs/reports/stage-19-http2-transport.md)
- [贡献指南](CONTRIBUTING.md)
- [变更记录](CHANGELOG.md)
- [技术设计](docs/mini-rpc-technical-design.md)
- [批次执行计划](docs/execution-plan.md)

## HTTP/2 一元 RPC

Echo Server 和 Echo Client 可通过 `-http2` 选择 HTTP/2 一元传输。明文监听使用 h2c prior knowledge；配置 TLS 时通过 ALPN 协商 `h2`。每个 Mini-RPC v1 帧占用一个 HTTP/2 POST stream，`RequestID`、Metadata、deadline、取消、Trace、治理和错误帧语义保持不变：

```bash
go run ./cmd/echo-server -addr=127.0.0.1:8080 -http2
go run ./cmd/echo-client -addr=127.0.0.1:8080 -http2 -message="hello over h2c"
```

TLS 模式沿用现有证书参数，并要求客户端和服务端都协商 `h2`：

```bash
go run ./cmd/echo-server \
  -addr=127.0.0.1:8443 \
  -http2 \
  -tls-cert=/path/server.crt \
  -tls-key=/path/server.key

go run ./cmd/echo-client \
  -addr=127.0.0.1:8443 \
  -http2 \
  -tls-ca=/path/ca.crt
```

运行容器化发现验收：

```bash
make docker-http2-test
```

该测试验证 h2c 服务端发现、生成 Echo RPC、共享连接上的并发流和 Prometheus 指标。当前 HTTP/2 路径只支持一元 RPC，不兼容 gRPC，也不提供浏览器 HTTP/1 fallback；完整边界见 [HTTP/2 一元传输](docs/http2.md)。

## Docker Compose 发现环境

下面的命令会启动 etcd、两个注册到 etcd 的 Echo Server 和一个 Prometheus：

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml ps
```

完整 key 布局、Lease 和 Resolver 语义见[服务注册与发现](docs/registry.md)。

两个服务端指标分别映射到 `http://127.0.0.1:9090/metrics` 和 `http://127.0.0.1:9091/metrics`，Prometheus UI 位于 `http://127.0.0.1:9092`。抓取配置和查询示例见[可观测性](docs/observability.md)。

运行一次可重复的容器 smoke test：

```bash
make docker-test
```

该命令会构建 Server/Client 镜像，通过 etcd 服务发现发起真实 Echo RPC，验证 `rpc_requests_total`，结束后删除本次 Compose project 的容器、网络、卷和本地构建镜像。需要手动运行客户端时，可使用 `client` profile：

```bash
docker compose -f deploy/docker-compose.yml --profile client run --rm echo-client \
  -etcd-endpoints=etcd:2379 -message="hello from compose"
```

所有宿主机端口默认绑定 `127.0.0.1`。可以用 `MINIRPC_ETCD_PORT`、`MINIRPC_ECHO_A_METRICS_PORT`、`MINIRPC_ECHO_B_METRICS_PORT`、`MINIRPC_PROMETHEUS_PORT` 和 `MINIRPC_OTLP_HTTP_PORT` 覆盖本地端口。

## RPC TLS 与 mTLS

Echo Server 设置证书和私钥后只接受 TLS 连接；再设置客户端 CA 即启用双向 TLS：

```bash
go run ./cmd/echo-server \
  -addr=127.0.0.1:8443 \
  -tls-cert=/path/server.crt \
  -tls-key=/path/server.key \
  -tls-client-ca=/path/ca.crt
```

客户端验证服务端证书，并提供 mTLS 客户端证书：

```bash
go run ./cmd/echo-client \
  -addr=127.0.0.1:8443 \
  -tls-ca=/path/ca.crt \
  -tls-cert=/path/client.crt \
  -tls-key=/path/client.key
```

默认最低 TLS 版本为 1.2。服务发现模式下，客户端默认用每个实例地址的 host 校验证书；证书必须覆盖所有可能发布的服务名或 IP。完整参数、证书模型、Docker mTLS 验收和安全边界见[RPC TLS 与双向 TLS](docs/tls-mtls.md)。

服务端和客户端默认每 `10s` 检查文件内容；内容未变不会创建新版本。更新会先完整解析并验证，成功后原子发布给后续握手或 Dial；失败时保留 last-known-good 配置并增加失败指标。设置 `-tls-reload-interval=500ms` 可调整周期，`0` 则关闭轮询。已有连接不会被中途改写或主动断开。

## OpenTelemetry OTLP Trace

Echo Server 和 Echo Client 可以通过 OTLP/HTTP 导出关联 Span。端点为空时完全禁用导出；配置后客户端 Span 为 `rpc.client/<service>/<method>`，服务端 Span 为 `rpc.server/<service>/<method>`，服务端使用客户端 Metadata 中的远程父 Span：

```bash
go run ./cmd/echo-server \
  -addr 127.0.0.1:8080 \
  -otlp-endpoint https://collector.example.com/v1/traces \
  -otlp-sample-ratio 0.1

go run ./cmd/echo-client \
  -addr 127.0.0.1:8080 \
  -otlp-endpoint https://collector.example.com/v1/traces \
  -otlp-sample-ratio 0.1
```

生产默认使用 HTTPS 和系统可信根。明文 HTTP Collector 必须显式增加 `-otlp-insecure`，HTTPS 与 `-otlp-insecure` 同时出现会拒绝启动；URL 内嵌凭据和 fragment 同样会被拒绝。认证 Header 可通过 `-otlp-headers 'authorization=Bearer ...'` 注入，错误日志不会回显 Header 值。

私有 CA 或 Collector mTLS 使用独立于 RPC 通道的 OTLP TLS 参数：

```bash
go run ./cmd/echo-server \
  -addr 127.0.0.1:8080 \
  -otlp-endpoint https://collector.example.com/v1/traces \
  -otlp-ca /path/collector-ca.crt \
  -otlp-cert /path/otel-client.crt \
  -otlp-key /path/otel-client.key \
  -otlp-tls-server-name collector.example.com \
  -otlp-tls-min-version 1.2 \
  -otlp-tls-reload-interval 10s
```

`-otlp-ca` 替换系统根证书用于 Collector 验证，`-otlp-cert`/`-otlp-key` 提供客户端身份，`-otlp-tls-server-name` 明确 SAN 校验名称；留空时从 HTTPS Endpoint 的主机名推导。TLS 参数只允许与 HTTPS Endpoint 一起使用，且没有 `InsecureSkipVerify` 开关。

`-otlp-tls-reload-interval` 默认每 `10s` 轮询文件内容，`0` 关闭轮询。CA、客户端证书或私钥变化时，新配置必须先完整解析并与私钥匹配，随后原子替换 OTLP Exporter；失败时继续使用 last-known-good Exporter。轮换只影响后续导出，不改变已经完成的 RPC 或 Mini-RPC 帧。

采样使用 ParentBased + TraceIDRatioBased：根调用按 `-otlp-sample-ratio` 决策，服务端尊重客户端 `trace_flags`。客户端和服务端退出时会 flush batch；Trace ID、Span ID、采样位仍沿 Mini-RPC Metadata 传播，不改变 v1 帧格式。

本地验证可启动固定版本 Collector、etcd、两个 Echo Server 和 Echo Client：

```bash
make docker-otel-test
```

该测试要求 Collector 实际收到同一 Trace ID 的客户端和服务端 Span，并检查 `service.name`。完整配置、属性和安全边界见[可观测性](docs/observability.md)。

验证私有 CA、客户端身份和 Collector 端证书强制校验：

```bash
make docker-otel-mtls-test
```

该测试会生成一次性 CA、服务端证书和客户端证书，要求正常客户端成功完成 RPC 与 Trace 导出，同时确认不带客户端证书的导出收到 `tls: certificate required`。

验证运行中的 Echo Server 轮换 OTLP 根 CA 与客户端身份，并让重启后的 Collector 使用同一组新材料：

```bash
make docker-otel-mtls-rotation-test
```

测试还要求轮换前的 Trace 与轮换后的新 Trace 分别保持 client/server 关联，并确认退役客户端证书被 Collector 拒绝。

## 性能基准

完整默认 benchmark 每场景预热 500ms、测量 3s，覆盖 32B 到 4096B 明文串行调用、单连接/8 连接并发、双实例 Round Robin、加权 1:2、治理开关、TLS/mTLS 串行与并发调用，以及三种传输的连接建立成本：

```bash
make benchmark
CGO_ENABLED=0 go run -trimpath ./benchmark -json
```

只验证 plaintext、TLS 和 mTLS 基准链路时使用短跑：

```bash
make benchmark-smoke
```

发布结果、指标定义和“客户端与本地 Server 同进程”的边界见[性能基准](docs/benchmarking.md)。

## 完成状态

按设计文档的验收边界，v0.1.0 的 **13 个实现批次（阶段 0 至阶段 12）** 已全部完成。阶段报告位于 `docs/reports/`，版本变更见 [CHANGELOG.md](CHANGELOG.md)。

v0.2 的扩展批次 14（阶段 13：TLS/mTLS）、批次 15（阶段 14：TLS/mTLS 性能验收）、批次 16（阶段 15：TLS 证书热加载与安全轮换）、批次 17（阶段 16：OpenTelemetry OTLP Trace 导出）、批次 18（阶段 17：OTLP Collector 自定义 CA 与 mTLS）、批次 19（阶段 18：OTLP TLS 材料热加载与轮换）和批次 20（阶段 19：HTTP/2 一元传输）已完成；它们保持 v0.1.0 的协议和公开 API 兼容，并分别积累安全行为、加密传输性能、在线轮换、分布式 Trace、安全遥测链路、遥测凭据轮换和流级多路复用证据。

## 开发原则

- 一次只推进一个批次，并保持前置批次验收通过。
- 新增对外接口时同时补充测试和设计说明。
- 并发组件必须通过 `go test -race`。
- 每个阶段保留独立提交、测试结果、已知限制和下一阶段依赖。
- 最终以格式、vet、测试、竞态测试、构建、Docker Compose 集成测试和 benchmark 报告共同作为完成依据。

## License

[MIT](LICENSE)
