# Contributing to Mini-RPC

感谢参与 Mini-RPC。v0.1.0 的目标是保持协议、并发和治理边界可测试、可解释、可复现；提交改动前请先阅读[架构概览](docs/architecture.md)和[技术设计](docs/mini-rpc-technical-design.md)。

## 环境要求

- Go 1.23 或更高版本。`go.mod` 以 Go 1.23 为基线，低版本工具链不保证结果。
- GNU Make、Git 和支持 POSIX shell 的 Linux 环境。
- Protocol Buffers compiler 28.3 或兼容版本。
- 运行容器验收时需要 Docker 与 Compose v2。
- 运行 RPC TLS、证书轮换或 OTLP Collector mTLS Docker 验收时需要 OpenSSL。
- `golangci-lint` v1.64.8；本地缺少时可以跳过 `make lint`，但 CI 会执行同一版本。

Benchmark 的 CPU、Heap 和 RSS 采集依赖 Linux `/proc`；其他平台仍可运行功能测试，但性能资源字段不可直接用于发布报告。

## 初始化

```bash
go version
go env GOMOD
make tools
make proto
make check
```

`make tools` 将固定版本的 `protoc-gen-go` 和项目内置的 `protoc-gen-minirpc` 安装到被 Git 忽略的 `bin/`。不要手工修改 `api/` 下的生成文件；修改 `api/echo/v1/echo.proto` 后运行 `make proto`。

## 变更原则

- 优先在现有稳定边界内扩展：`pkg/rpc`、协议字节布局和 Resolver 快照是高返工成本接口。
- 新增公共行为时同时补测试和文档；并发状态机必须覆盖正常、取消、关闭和竞态路径。
- 自动重试只适用于可识别的临时错误和显式幂等调用。不要为未确认写入增加隐式重放。
- TLS 默认最低版本为 1.2；新增 TLS 行为必须测试可信证书、不可信 CA、服务器名称和 mTLS 缺失证书，不允许提供跳过验证的生产快捷开关。热加载必须先验证后发布，并保留 last-known-good 配置。
- 保留 metrics 标签的有界性；禁止把 request_id、trace_id 或任意用户输入加入标签。
- OTLP Endpoint 默认使用 HTTPS；明文 HTTP 只能通过显式 `-otlp-insecure` 开启。私有 CA 和客户端身份只允许用于 HTTPS，禁止 `InsecureSkipVerify` 或低于 TLS 1.2 的配置。exporter Header、认证 token、URL 凭据、证书路径和 Collector 私密配置不得进入日志、Metrics、测试快照或提交。
- Trace schema 保持低基数：service、method、role、outcome、error_code 可做属性；完整 request_id、trace_id 和 span_id 只作为日志关联字段或 OTel Span 内建 ID。
- 不提交调试输出、临时依赖、覆盖率或 benchmark 原始临时文件。
- 不重写已有 Git 历史；每个逻辑变更使用独立、可回滚的提交。

## 验证矩阵

提交前至少运行与改动范围对应的命令：

| 改动类型 | 必需检查 |
|---|---|
| 任意 Go 改动 | `gofmt -w <files>`、`make check` |
| 并发、连接、Resolver、熔断或生命周期 | `make race` |
| 代码风格和静态分析 | `golangci-lint run` |
| 故障切换、恢复或重试边界 | `make fault-test` |
| Benchmark 工具 | `go test -race ./benchmark`、`make benchmark-smoke` |
| Dockerfile、Compose 或部署入口 | `docker compose -f deploy/docker-compose.yml config --quiet`、`make docker-test` |
| TLS、证书加载、mTLS 或热轮换 | `go test -race ./internal/client ./internal/server ./internal/security`、`make docker-tls-test`、`make docker-tls-rotation-test` |
| OpenTelemetry、Trace 传播或 OTLP | `go test -race ./internal/observability ./internal/client ./internal/server`、`make docker-otel-test`、`make docker-otel-mtls-test` |

完整本地门禁：

```bash
make check
make race
golangci-lint run
go test -race ./benchmark
make fault-test
docker compose -f deploy/docker-compose.yml config --quiet
make docker-test
make docker-tls-test
make docker-tls-rotation-test
make docker-otel-test
make docker-otel-mtls-test
```

`make check` 会重新生成 Protobuf 代码，并在生成文件与提交内容不一致时失败。`make docker-test` 启动独立 Compose project，执行通过 etcd 发现选择的真实 Echo RPC 和指标抓取，结束后删除该 project 的容器、网络、卷以及本地构建镜像。

`make docker-tls-test` 额外生成一次性 CA、服务端证书和客户端证书，验证双向 TLS 发现调用、明文拒绝和握手失败指标。测试证书只能用于本地验收，禁止提交私钥或放宽到 `InsecureSkipVerify`。

`make docker-tls-rotation-test` 会在线替换服务端证书、私钥、客户端 CA、根 CA 和客户端身份，验证新握手使用新材料、退役客户端身份被拒绝、既有 Go 连接继续调用，以及无效服务端证书不会覆盖 last-known-good 配置。测试使用临时目录并在退出时删除全部私钥。

`make docker-otel-test` 启动固定版本的 OpenTelemetry Collector，通过真实 etcd 发现完成 Echo RPC，并从 Collector 的 debug exporter 检查客户端/服务端 Span、service resource 和共享 Trace ID。Collector 只在本机绑定随机可用端口，测试结束后清理自己的 Compose project。

`make docker-otel-mtls-test` 为 Collector 与 Echo 客户端/服务端生成一次性私有 CA、服务端证书和客户端证书。测试要求自定义根证书和服务名校验下 Trace 成功导出，并单独执行不带客户端身份的导出，确认 Collector 返回 `tls: certificate required`。测试材料位于临时目录，退出时删除；不得把生成证书或私钥加入版本库。

## Benchmark

默认 benchmark 每个场景 warmup 500ms、测量 3s，包含明文基线、TLS/mTLS 工作负载和三种传输的连接建立样本。完整运行时间随机器变化，不应在 CI 中把噪声较大的吞吐量作为通过/失败阈值。

```bash
make benchmark
CGO_ENABLED=0 go run -trimpath ./benchmark -json
```

选择传输或关闭建连采样：

```bash
CGO_ENABLED=0 go run -trimpath ./benchmark \
  -transports=plaintext,tls,mtls -handshakes=64
```

短跑仅适合验证工具本身：

```bash
CGO_ENABLED=0 go run -trimpath ./benchmark \
  -duration=150ms -warmup=20ms -concurrency=16 -sizes=32 -handshakes=10
```

发布性能结论必须记录 commit SHA、Go 版本、机器、编译参数、消息大小、并发数、连接数、实例数、传输、QPS、P50/P95/P99、建连分位和错误率，并注明 CPU/Heap/RSS 是同一进程内客户端与本地 Server 的合计值。不要只挑单次最好结果。详细方法见[性能基准](docs/benchmarking.md)。

## 提交规范

提交信息使用 Conventional Commits：

```text
<type>(<scope>): <imperative summary>

Explain why the change is needed, the behavior boundary that changed, and the
tests and documentation that provide evidence. Mention known limitations when
the implementation intentionally stops short of a broader production concern.
```

常用 type 包括 `feat`、`fix`、`perf`、`test`、`docs`、`build`、`ci` 和 `refactor`。提交正文应说明用户可见行为、并发/兼容性影响和验证方式，避免只重复标题。

## 文档与阶段证据

- 专题行为写入 `docs/`，不要只留在提交记录中。
- 每个里程碑在 `docs/reports/` 记录实现、命令、真实结果、验收项、已知限制和下一阶段依赖。
- 发布说明从 `CHANGELOG.md` 更新；README 只承诺已经实现并验证的命令。
- 性能或故障结果必须保留环境与 commit SHA，避免把不可复现的单次数字当作长期容量承诺。
