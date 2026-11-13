# Mini-RPC

Mini-RPC 是一个面向 Go 微服务场景的可治理 RPC 框架项目。v0.1.0 覆盖 Protobuf 接口与代码生成、TCP 长连接与一元 RPC、服务注册发现、负载均衡、超时取消、重试熔断、令牌桶限流、可观测性、优雅关闭，以及集成测试、故障注入和性能基准；v0.2 扩展继续加入 RPC TLS 与 mTLS。

稳定基线为 **v0.1.0**，13 个实现批次已全部完成；**v0.2 开发线**已完成 TLS/mTLS 传输安全扩展。仓库具备生成代码、二进制帧协议、单连接并发调用、每实例有界连接池、etcd Lease/KeepAlive/Watch、本地实例快照、Watch 重连、Round Robin、加权 Round Robin、deadline 透传、CANCEL 帧、有限幂等重试、退避与熔断、令牌桶和资源保护、结构化日志、Trace、Prometheus 指标、GracefulStop、故障切换、性能基准、Docker Compose，以及证书验证和双向 TLS。

## 环境要求

- Go 1.23 或更高版本
- GNU Make
- Git
- Protocol Buffers compiler（`protoc` 28.3 或兼容版本）
- Docker 与 Compose v2（仅运行容器验收时需要）
- OpenSSL（仅运行 `make docker-tls-test` 的测试证书生成时需要）

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
| `make benchmark` | 运行十个真实 Echo 场景并输出 QPS、延迟分位和资源数据 |
| `make docker-test` | 构建 Compose 环境，执行发现 RPC 和指标 smoke test 后清理资源 |
| `make docker-tls-test` | 生成一次性证书，验证发现、RPC mTLS 和握手失败指标后清理资源 |
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
│   ├── observability/    # 日志、指标和 Trace
│   ├── protocol/         # 二进制帧协议
│   ├── registry/         # etcd 等注册中心适配器
│   ├── resolver/         # 客户端实例快照
│   ├── server/           # 服务端分发与生命周期
│   ├── security/         # RPC TLS 证书与配置加载
│   ├── testkit/          # 测试服务器、代理和故障注入工具
│   └── transport/        # TCP 连接与请求关联
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
- [贡献指南](CONTRIBUTING.md)
- [变更记录](CHANGELOG.md)
- [技术设计](docs/mini-rpc-technical-design.md)
- [批次执行计划](docs/execution-plan.md)

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

所有宿主机端口默认绑定 `127.0.0.1`。可以用 `MINIRPC_ETCD_PORT`、`MINIRPC_ECHO_A_METRICS_PORT`、`MINIRPC_ECHO_B_METRICS_PORT` 和 `MINIRPC_PROMETHEUS_PORT` 覆盖本地端口。

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

## 性能基准

完整默认 benchmark 每场景预热 500ms、测量 3s，覆盖 32B 到 4096B 串行调用、单连接/8 连接并发、双实例 Round Robin、加权 1:2 和治理开关：

```bash
make benchmark
CGO_ENABLED=0 go run -trimpath ./benchmark -json
```

发布结果、指标定义和“客户端与本地 Server 同进程”的边界见[性能基准](docs/benchmarking.md)。

## 完成状态

按设计文档的验收边界，v0.1.0 的 **13 个实现批次（阶段 0 至阶段 12）** 已全部完成。阶段报告位于 `docs/reports/`，版本变更见 [CHANGELOG.md](CHANGELOG.md)。

v0.2 的扩展批次 14（阶段 13：TLS/mTLS）也已完成；它保持 v0.1.0 的协议和公开 API 兼容，并为后续安全发布继续积累独立验收证据。

## 开发原则

- 一次只推进一个批次，并保持前置批次验收通过。
- 新增对外接口时同时补充测试和设计说明。
- 并发组件必须通过 `go test -race`。
- 每个阶段保留独立提交、测试结果、已知限制和下一阶段依赖。
- 最终以格式、vet、测试、竞态测试、构建、Docker Compose 集成测试和 benchmark 报告共同作为完成依据。

## License

[MIT](LICENSE)
