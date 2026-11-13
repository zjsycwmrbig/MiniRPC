# 可观测性

阶段 10 在现有调用链中加入进程内结构化日志、请求关联标识、轻量 Span 和 Prometheus 文本指标。实现目标是先建立稳定、低依赖的诊断闭环：一次调用的客户端与服务端日志可以通过同一 `trace_id` 关联，指标可以区分成功、失败、超时、取消和资源耗尽，并且关闭观测后不改变 RPC 语义。

当前实现不依赖 OpenTelemetry SDK，也不包含 OTLP Exporter、Tempo、Jaeger 或外部日志平台。Span 是进程内生命周期对象，调用完成后主要沉淀为 JSON 日志和指标。

## 启用方式

客户端和服务端都通过 `internal/observability.Runtime` 共享同一套标识、日志和指标能力：

```go
observer := observability.New(observability.Config{
    Enabled:  true,
    Debug:    false,
    Instance: "echo-server-1",
    Output:   os.Stderr,
})

rpcServer, err := server.NewWithOptions(server.WithObserver(observer))
clientConn, err := client.Dial(ctx, address, client.WithObserver(observer))
```

Echo Server 默认开启观测并监听 `127.0.0.1:9090/metrics`：

```bash
go run ./cmd/echo-server \
  -addr 127.0.0.1:8080 \
  -metrics-addr 127.0.0.1:9090 \
  -instance-id echo-server-1
```

Echo Client 默认开启观测：

```bash
go run ./cmd/echo-client \
  -addr 127.0.0.1:8080 \
  -message "hello observability" \
  -instance echo-client-1
```

抓取指标：

```bash
curl -sS http://127.0.0.1:9090/metrics
```

关闭观测：

```bash
go run ./cmd/echo-server -observability=false -metrics-addr=""
go run ./cmd/echo-client -observability=false
```

关闭后客户端不生成 `request_id`、`trace_id` 或 `span_id`，不写观测日志，不记录指标；服务端也不启动 `/metrics` HTTP Server。RPC、重试、熔断和限流仍按原配置运行。

## 调用关联

客户端 `Invoke` 创建根 Span，并生成：

- `request_id`：32 个十六进制字符；同一次调用内的全部 retry attempt 共用一个值。
- `trace_id`：32 个十六进制字符，对应 W3C 128-bit Trace ID。
- `span_id`：16 个十六进制字符，对应 64-bit Span ID。

标识通过请求 Metadata 传播：

```text
客户端 Invoke
  -> 创建 client span，生成 request_id/trace_id/span_id
  -> Request Metadata 写入 request_id、trace_id、span_id
  -> 服务端从 Metadata 读取并创建 server span
       trace_id    = 客户端 trace_id
       parent_span_id = 客户端 span_id
       request_id  = 客户端 request_id
  -> Handler 完成后分别写客户端和服务端完成日志
```

服务端在 Metadata 缺失或 ID 格式错误时创建新标识，不会因为追踪字段非法拒绝 RPC。客户端预置了 `SpanContext.Traceparent()` 用于生成 W3C `00-<trace-id>-<span-id>-01` 字符串，但当前线上协议仍使用 Mini-RPC Metadata，不解析 W3C `traceparent`，也不包含采样位和 Baggage。

一个调用链示例：

```json
{"level":"INFO","msg":"rpc.call.completed","role":"client","service":"rpc.echo.v1.Echo","method":"Echo","request_id":"9874d3c53956d10761917555e5664fe9","trace_id":"da32ac0a724a37984e07e8dd04cf57a6","span_id":"80467bf4b77ee304","parent_span_id":"","instance":"echo-observe-client","attempt":1,"outcome":"success","error_code":"ok"}
{"level":"INFO","msg":"rpc.call.completed","role":"server","service":"rpc.echo.v1.Echo","method":"Echo","request_id":"9874d3c53956d10761917555e5664fe9","trace_id":"da32ac0a724a37984e07e8dd04cf57a6","span_id":"9e9065be5bacfd19","parent_span_id":"80467bf4b77ee304","instance":"echo-observability-demo","attempt":1,"outcome":"success","error_code":"ok"}
```

两条日志的 `trace_id` 和 `request_id` 相同，服务端 `parent_span_id` 等于客户端 `span_id`。

## JSON 日志

`log/slog` 使用 JSON Handler 输出到配置的 Writer，默认是 `os.Stderr`。完成事件始终以 `INFO` 写入；只有 `Debug=true` 时才额外输出 `rpc.call.start`。

| 字段 | 说明 |
|---|---|
| `time` | 日志时间 |
| `level` | `INFO` 或 `DEBUG` |
| `msg`、`event` | `rpc.call.start` 或 `rpc.call.completed` |
| `role` | `client` 或 `server` |
| `service`、`method` | 稳定路由标签；未知路由使用 `unknown` |
| `request_id` | 一次调用及全部 attempt 的关联 ID |
| `trace_id` | 跨客户端和服务端的 Trace ID |
| `span_id` | 当前客户端或服务端 Span ID |
| `parent_span_id` | 上游 Span ID；客户端根 Span 为空 |
| `instance` | 进程或容器实例名 |
| `attempt` | 调用实际执行的 attempt 数，至少为 1 |
| `duration_ms` | Span 总耗时，单位毫秒 |
| `outcome` | `success`、`failure`、`timeout`、`canceled` 或 `rate_limited` |
| `error_code` | 稳定 RPC 错误码；成功为 `ok` |
| `error` | 失败时的错误消息，成功时省略 |

客户端 Span 覆盖序列化、熔断和限流准入、连接池、全部 retry attempt、退避等待及最终响应处理。因此客户端耗时是调用总耗时，不是单次网络往返耗时。服务端 Span 从并发准入后的分发开始，覆盖参数校验、请求体限制、限流、解码、Handler 和编码。

## Prometheus 指标

`/metrics` 只接受 `GET` 和 `HEAD`，返回：

```text
Content-Type: text/plain; version=0.0.4; charset=utf-8
```

其他 HTTP 方法返回 `405 Method Not Allowed`。

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `rpc_requests_total` | Counter | `role`、`service`、`method`、`outcome` | 完成的 RPC 调用数 |
| `rpc_requests_failed_total` | Counter | `role`、`service`、`method`、`error_code` | 未成功调用按稳定错误码计数 |
| `rpc_request_duration_seconds` | Histogram | `role`、`service`、`method` | 调用总耗时分布 |
| `rpc_inflight_requests` | Gauge | `role` | 当前执行中的调用数 |
| `rpc_connections` | Gauge | `role` | 当前活动的连接池连接或服务端 TCP 连接数 |
| `rpc_retries_total` | Counter | `role`、`service`、`method` | 重试产生的额外 attempt 数 |
| `rpc_circuit_state` | Gauge | `role`、`state` | 当前客户端熔断状态；当前状态系列值为 1 |
| `rpc_rate_limited_total` | Counter | `role`、`service`、`method` | `RESOURCE_EXHAUSTED` 结果数 |
| `rpc_registry_instances` | Gauge | `service` | 当前进程已注册的本地服务实例数 |

耗时 Histogram 的桶边界为：

```text
0.001, 0.0025, 0.005, 0.01, 0.025, 0.05,
0.1, 0.25, 0.5, 1, 2.5, 5, 10, +Inf
```

`rpc_requests_total` 的 `outcome` 分类如下：

| outcome | 来源 |
|---|---|
| `success` | `error_code=ok` |
| `failure` | 其他稳定失败码，例如 `internal`、`unavailable`、`not_found` |
| `timeout` | `deadline_exceeded` |
| `canceled` | `canceled` |
| `rate_limited` | 所有 `resource_exhausted` |

`rate_limited` 是资源耗尽类统一分类，不只代表令牌桶拒绝；服务端并发和请求体限制也会产生该结果。连接数超限通过关闭 TCP 表达，调用方通常观察到 `unavailable`。`rpc_requests_failed_total` 与 `rpc_rate_limited_total` 存在有意重叠：前者按错误码排查，后者提供资源耗尽专用面板和告警查询。

服务端未知 service 或 method 被记为 `unknown`，防止恶意或错误路由直接放大标签数量。指标禁止使用 `request_id`、`trace_id`、`span_id` 等高基数字段；完整关联 ID 只存在于日志。

## 基数边界

每个进程最多保留 512 个 `role/service/method` 路由组合，超过上限的新组合统一聚合为：

```text
service="__overflow__",method="__overflow__"
```

已登记路由继续使用原标签。`rpc_registry_instances` 的 service 系列同样最多 512 个。`outcome`、`error_code`、`role` 和熔断状态来自固定集合。指标注册表使用互斥锁保护并发写入；抓取时先在锁内复制快照，释放锁后再渲染，慢速导出器不会在序列化期间长期阻塞 RPC 记录。每次抓取都创建独立快照，不影响计数器语义，也不读取或修改限流桶令牌。

## 本地抓取

启动 Echo Server 和 Client，然后查询常用指标：

```bash
curl -sS http://127.0.0.1:9090/metrics \
  | grep -E 'rpc_(requests|inflight|connections|retries|circuit)'
```

示例查询：

```promql
sum by (service, method, outcome) (rate(rpc_requests_total[5m]))
sum by (service, method) (rate(rpc_requests_failed_total[5m]))
histogram_quantile(0.95, sum by (le, service, method) (rate(rpc_request_duration_seconds_bucket[5m])))
rpc_inflight_requests{role="server"}
rpc_circuit_state{state="open"}
```

## Docker Compose 抓取

`deploy/docker-compose.yml` 启动 etcd、两个 Echo Server 和一个 Prometheus。两个服务端在容器内监听 `0.0.0.0:9090`，宿主机分别映射到 `9090` 和 `9091`；Prometheus 映射到 `9092`。

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml ps
curl -sS http://127.0.0.1:9090/metrics
curl -sS http://127.0.0.1:9091/metrics
```

浏览器访问 `http://127.0.0.1:9092` 打开 Prometheus UI。抓取配置见 `deploy/prometheus.yml`：

```yaml
global:
  scrape_interval: 5s

scrape_configs:
  - job_name: mini-rpc
    static_configs:
      - targets:
          - echo-a:9090
          - echo-b:9090
```

停止环境：

```bash
docker compose -f deploy/docker-compose.yml down
```

## 测试覆盖

- 客户端和服务端使用同一 `trace_id`、`request_id`，服务端 `parent_span_id` 正确关联客户端 Span。
- 每个完成 Span 都产生一条 JSON 日志，失败码和 outcome 分类正确。
- `Debug=false` 不输出开始事件，`Debug=true` 输出开始与完成事件。
- 指标区分成功、失败、超时、取消和限流，Histogram 与重试计数正确。
- 指标标签不包含 `request_id` 或 `trace_id`。
- 512 个路由组合上限和 overflow 聚合生效。
- 并发写入、并发抓取以及关闭观测后的 RPC 路径通过 race detector。
- `/metrics` 的 `GET`、`HEAD` 和非法方法行为通过 HTTP Handler 测试。
- 真实 Echo Server 与 Echo Client 通过日志和 `/metrics` 完成端到端验证。

## 已知限制

- Span 和指标保存在单进程内，进程重启后清零，不提供持久化或跨进程聚合。
- 当前没有 OTLP Exporter、采样、Baggage、Span 事件或分布式 Trace 后端接入。
- Mini-RPC 使用自有 Metadata 传播 Trace ID；`Traceparent()` 只提供格式转换能力。
- 客户端耗时包含 retry 和退避，当前没有逐 attempt 独立 Histogram。
- `rpc_circuit_state` 一次只导出一个当前状态系列，状态切换后旧状态系列消失。
- Metrics 没有 TLS、鉴权、限流或租户隔离；默认仅监听回环地址，容器场景必须显式改为 `0.0.0.0`。
- 日志只保证协议层关联字段稳定，不包含业务请求体、响应体或敏感字段脱敏策略。
- 尚未提供 Grafana Dashboard、Alert Rules 和长期存储配置。
