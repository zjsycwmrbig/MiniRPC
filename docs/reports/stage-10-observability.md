# 阶段 10 报告：可观测性

## 阶段

阶段 10：接入 JSON 结构化日志、request/trace/span 关联、客户端与服务端 Span、Prometheus 文本指标、调试开关和有界标签，并验证关闭观测时 RPC 语义不变。

## 相关提交

- `8f5baa3 feat(observability): add structured logs metrics and trace correlation`
- `19d5747 docs(observability): document metric labels trace propagation and scrape setup`

本报告使用独立文档提交记录测试证据、验收结果和阶段 11 依赖，不改变已提交的实现与语义基线。

## 实现

- 新增 `internal/observability.Runtime`，使用 `log/slog` JSON Handler 输出结构化日志，并管理 Span 和进程内指标。
- 客户端每次 `Invoke` 生成 `request_id`、`trace_id` 和根 `span_id`；同一次调用的全部 retry attempt 复用同一组关联 ID。
- 关联 ID 通过 `request_id`、`trace_id`、`span_id` Metadata 传播；服务端创建子 Span，继承 `trace_id` 和 `request_id`，并将客户端 Span ID 写为 `parent_span_id`。
- Metadata 缺失、Trace ID 全零、Span ID 长度或十六进制格式错误时，服务端生成合法的新标识，不拒绝 RPC。
- 客户端 Span 覆盖序列化、熔断准入、本地限流、连接池、全部 attempt、退避和响应处理。
- 服务端 Span 覆盖分发、参数与请求体校验、限流、解码、Handler、编码和 panic 恢复；早期的 draining 与并发拒绝也记录独立的资源耗尽结果。
- 完成日志固定包含 role、service、method、request_id、trace_id、span_id、parent_span_id、instance、attempt、duration_ms、outcome、error_code 和失败消息。
- `Debug=false` 只输出完成事件；`Debug=true` 额外输出 `rpc.call.start`。
- `Enabled=false` 时不生成 ID、不写日志、不记录指标；Echo Server 也不会启动 `/metrics` HTTP Server。
- 新增 Prometheus 文本指标：请求结果、失败错误码、耗时 Histogram、inflight、连接数、额外 retry attempt、熔断状态、资源耗尽和注册实例数。
- 成功、失败、超时、取消和资源耗尽分别映射为 `success`、`failure`、`timeout`、`canceled` 和 `rate_limited`。
- 指标只使用 role、service、method、outcome、error_code、state 等有界标签，不导出 request_id 或 trace_id。
- 每个进程最多保留 512 个 role/service/method 路由组合；未知路由使用 `unknown`，超限的新组合聚合为 `__overflow__`。
- 抓取时先在锁内复制全部指标和 Histogram 桶，释放锁后再渲染，慢速导出器不会在序列化期间长期阻塞请求记录。
- `/metrics` 支持 GET 和 HEAD，其他方法返回 405。
- Echo Server 增加 `-observability`、`-debug` 和 `-metrics-addr`；Echo Client 增加 `-observability`、`-debug` 和 `-instance`。
- Docker Compose 增加两个服务端 metrics 端口映射、容器内 `0.0.0.0:9090` 监听和 Prometheus 服务；新增 `deploy/prometheus.yml`。
- 新增 `docs/observability.md`，说明日志字段、Trace 传播、指标名称与标签、基数边界、PromQL、本地抓取、Docker Compose 和已知限制。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过 |
| `golangci-lint run` | 通过 |
| `go test -race -count=3 ./internal/observability ./internal/client ./internal/server` | 通过 |
| `docker compose -f deploy/docker-compose.yml config --quiet` | 通过 |
| Echo Server/Client 真实进程调用 | 通过，两端日志可通过 trace_id 与 request_id 关联 |
| `/metrics` 真实 HTTP 抓取 | 通过，返回 200、Prometheus Content-Type 和成功请求指标；非法方法返回 405 |

关键测试结果：

- `TestSpanContinuesTraceAndCorrelatesStructuredLogs`：客户端和服务端 Trace、Request ID 一致，服务端 parent_span_id 等于客户端 span_id。
- `TestObservabilityCorrelatesClientAndServerCalls`：通过真实 TCP Server/Client 调用验证日志关联和双端指标导出。
- `TestDisabledObservabilityDoesNotChangeRPC`：关闭观测后 Echo 调用成功，客户端和服务端均不输出日志。
- `TestDebugSwitchControlsStartEvents`：Debug 开关准确控制开始事件，完成事件始终输出。
- `TestMetricsClassifySuccessFailureTimeoutAndRateLimit`：请求、失败、耗时、重试、连接、熔断、限流和注册实例指标均存在且分类正确。
- `TestMetricsBoundServiceMethodCardinality`：第 513 个新路由进入 overflow 聚合，不出现未受限 method 标签。
- `TestMetricsConcurrentRecordAndRender`：并发写入和抓取通过 race detector。
- `TestPrometheusHandlerAndDisabledRuntime`：GET、HEAD、405、Content-Type 和关闭观测的 no-op 行为正确。
- `TestTraceMetadataRoundTripAndInvalidIDs`：合法 ID 往返和 W3C traceparent 生成正确，非法 ID 被拒绝。

真实调用日志证据：

```text
client trace_id=0f7fad01243632de94e91d4e0655f476 span_id=35748a545c6cab18 request_id=e719e1492405820d6417dc99e85fc904
server trace_id=0f7fad01243632de94e91d4e0655f476 span_id=5435bae4b9d85564 parent_span_id=35748a545c6cab18 request_id=e719e1492405820d6417dc99e85fc904
```

真实指标证据：

```text
rpc_requests_total{role="server",service="rpc.echo.v1.Echo",method="Echo",outcome="success"} 1
rpc_request_duration_seconds_count{role="server",service="rpc.echo.v1.Echo",method="Echo"} 1
rpc_inflight_requests{role="server"} 0
```

## 验收

- [x] 每次启用观测的调用都有 request_id。
- [x] 客户端和服务端日志能够通过 trace_id、request_id 关联。
- [x] 服务端 Span 正确引用客户端 parent_span_id。
- [x] 成功、失败、超时、取消和限流分别计数。
- [x] 指标标签不含 request_id 或 trace_id，service/method 基数有明确上限。
- [x] 指标包含请求、错误、耗时、重试、连接、inflight、熔断和资源耗尽状态。
- [x] 提供调试开关和完全关闭观测的 no-op 路径。
- [x] 关闭观测时 RPC 正常运行且不生成日志或指标。
- [x] `/metrics` 可被 Prometheus 文本格式抓取。
- [x] 提供本地 curl、PromQL 和 Docker Compose Prometheus 示例。
- [x] `docs/observability.md` 完整说明指标名称、类型、标签和关联方式。
- [x] 实现、说明和阶段报告使用独立语义提交。
- [x] GitHub Actions 远端运行成功：[run 43395105636](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43395105636)。

## 已知限制

- Span 和指标均为单进程内存状态，进程重启后清零，不提供跨进程聚合或持久化。
- 当前没有 OpenTelemetry SDK、OTLP Exporter、采样、Baggage、Span Event 或 Trace 后端。
- Mini-RPC 通过自有 Metadata 传播 trace_id 和 span_id；`Traceparent()` 只提供 W3C 格式转换，不解析外部 traceparent。
- 客户端耗时为调用总耗时，包含全部 retry 和退避；尚未按 attempt 记录独立 Histogram。
- `rpc_circuit_state` 只导出值为 1 的当前状态系列，状态切换后旧状态系列消失。
- `resource_exhausted` 统一映射为 `rate_limited`，因为当前稳定错误码无法区分令牌桶、并发和请求体拒绝；连接数超限通常表现为 `unavailable`。
- `/metrics` 没有 TLS、鉴权和应用层限流，默认只监听回环地址。
- 尚未提供 Grafana Dashboard、Prometheus Alert Rules 和长期存储配置。

## 下一阶段依赖

阶段 11 故障注入应复用本阶段的 request_id、trace_id 和指标观察恢复过程：

- 延迟、错误响应和断连场景应按同一 request_id 观察 retry attempt、最终错误码和恢复耗时。
- 单实例停止与恢复应结合注册实例数、重试数和连接数变化判断故障切换是否完成。
- 资源耗尽场景验证 `rate_limited` 与 `resource_exhausted` 的计数一致性，避免重试放大限流拒绝。
- etcd 短暂不可用应通过结构化日志和注册实例数观察 Watch/Lease 恢复，不允许形成重试或熔断死循环。
- 故障场景需要在测试结束后清理 Server、Client、Metrics HTTP Server 和临时监听端口，保证 race detector 无泄漏报告。
