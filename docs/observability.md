# 可观测性

阶段 10 在调用链中加入进程内结构化日志、请求关联标识、轻量 Span 和 Prometheus 文本指标。阶段 16 在不修改 v1 帧协议和公开 RPC API 的前提下增加标准 OpenTelemetry Span 与 OTLP/HTTP 批量导出。一次调用的客户端和服务端现在既可通过 JSON `trace_id` 关联，也可作为父子 Span 进入兼容 OTLP 的 Trace 后端。

OpenTelemetry 导出是可选的：`-otlp-endpoint` 为空时 SDK 不创建 TracerProvider，进程内日志、指标及自有关联 ID 保持原行为；配置后由 OpenTelemetry TracerProvider 生成或继承 Trace ID，并在完成时写入 JSON 日志和 OTLP batch。

## 启用方式

客户端和服务端都通过 `internal/observability.Runtime` 共享同一套标识、日志和指标能力：

```go
observer := observability.New(observability.Config{
    Enabled:     true,
    Debug:       false,
    Instance:    "echo-server-1",
    ServiceName: "mini-rpc-echo-server",
    Output:      os.Stderr,
    // TraceExporter 由 NewOTLPTraceExporter 创建；nil 表示不导出 OTel Span。
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
- `trace_flags`：两位十六进制采样位；`01` 表示采样，`00` 表示不采样。

标识通过请求 Metadata 传播：

```text
客户端 Invoke
  -> 创建 OpenTelemetry client span，生成 request_id/trace_id/span_id/trace_flags
  -> Request Metadata 写入 request_id、trace_id、span_id、trace_flags
  -> 服务端从 Metadata 读取并创建 server span
       trace_id    = 客户端 trace_id
       parent_span_id = 客户端 span_id
       request_id  = 客户端 request_id
       sampled     = 客户端 trace_flags == 01
  -> Handler 完成后分别写客户端和服务端完成日志
```

服务端在 Metadata 缺失或 ID 格式错误时创建新标识，不会因为追踪字段非法拒绝 RPC。`SpanContext.Traceparent()` 可生成带真实采样位的 W3C `00-<trace-id>-<span-id>-<flags>` 字符串，但线上协议仍使用 Mini-RPC Metadata，不解析或发送 W3C `traceparent` Header，也不包含 Baggage。

一个调用链示例：

```json
{"level":"INFO","msg":"rpc.call.completed","role":"client","service":"rpc.echo.v1.Echo","method":"Echo","request_id":"9874d3c53956d10761917555e5664fe9","trace_id":"da32ac0a724a37984e07e8dd04cf57a6","span_id":"80467bf4b77ee304","parent_span_id":"","instance":"echo-observe-client","attempt":1,"outcome":"success","error_code":"ok"}
{"level":"INFO","msg":"rpc.call.completed","role":"server","service":"rpc.echo.v1.Echo","method":"Echo","request_id":"9874d3c53956d10761917555e5664fe9","trace_id":"da32ac0a724a37984e07e8dd04cf57a6","span_id":"9e9065be5bacfd19","parent_span_id":"80467bf4b77ee304","instance":"echo-observability-demo","attempt":1,"outcome":"success","error_code":"ok"}
```

两条日志的 `trace_id` 和 `request_id` 相同，服务端 `parent_span_id` 等于客户端 `span_id`。

## OpenTelemetry OTLP

Echo Server 和 Echo Client 支持以下参数：

| 参数 | 默认值 | 行为 |
|---|---|---|
| `-otlp-endpoint` | 空 | OTLP/HTTP Trace Endpoint URL；空值完全禁用 OTel 导出 |
| `-otlp-insecure` | `false` | 允许明文 HTTP；只对显式 `http://` Endpoint 有效 |
| `-otlp-headers` | 空 | 逗号分隔的静态 `key=value` Header，例如 `authorization=Bearer token` |
| `-otlp-sample-ratio` | `1` | 本地根 Trace 采样比例，取值 `[0,1]` |
| `-otlp-export-timeout` | `5s` | 单次 batch 导出和退出 flush 的最大时间 |

启用示例：

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

Span 使用 OpenTelemetry Client/Server kind，并通过远程父 Span 形成同一条 Trace：

| 字段 | 客户端 | 服务端 |
|---|---|---|
| Span 名称 | `rpc.client/<service>/<method>` | `rpc.server/<service>/<method>` |
| Span kind | `Client` | `Server` |
| Trace ID | 根调用新建，或继承外层 OTel Context | 来自请求 Metadata |
| 父 Span | 外层 OTel Span；根调用为空 | 客户端 RPC Span |

稳定的低基数 Span 属性：

| 属性 | 示例 |
|---|---|
| `rpc.system` | `minirpc` |
| `rpc.service` | `rpc.echo.v1.Echo` |
| `rpc.method` | `Echo` |
| `rpc.minirpc.role` | `client` 或 `server` |
| `rpc.minirpc.attempts` | `1` |
| `rpc.minirpc.outcome` | `success`、`failure`、`timeout`、`canceled` 或 `rate_limited` |
| `rpc.minirpc.error_code` | `ok`、`internal`、`unavailable` 等稳定错误码 |

失败 RPC 将 Span Status 设为 `Error`，状态消息只包含稳定错误码。Resource 默认包含 `service.name`，Echo 示例分别为 `mini-rpc-echo-server` 和 `mini-rpc-echo-client`；`service.version` 与 `service.instance.id` 可在 `observability.Config` 中配置，Echo CLI 当前设置实例 ID。完整 Trace ID 和 Span ID 使用 OTel Span 内建字段，不复制为 Metrics 标签。

### 采样

TracerProvider 使用 `ParentBased(TraceIDRatioBased(ratio))`：

- 没有上游父 Span 时，按 `-otlp-sample-ratio` 做成比例采样。
- 服务端收到有效远程父 Span 时继承客户端采样决定，避免客户端采样而服务端丢 Span。
- 未采样 Span 不进入 Exporter，但 Trace/Span ID 和 `trace_flags=00` 仍传播到日志与下游 Metadata，便于诊断采样边界。

### OTLP 安全边界

- Endpoint 必须是带 host 的绝对 URL，只允许 `http` 或 `https`。
- `http` 必须同时显式设置 `-otlp-insecure`；`https` 设置 insecure 会拒绝启动。
- URL 内嵌用户名/密码和 fragment 会被拒绝，避免凭据出现在错误文本或配置面板。
- Header 名称和值启动时严格验证，重复 key 和非法的换行/控制字符被拒绝；错误只回显 Header 名称，不输出值。
- Header、Endpoint 查询参数和其他 Collector 私密配置不得进入日志或 Metrics。
- HTTPS Exporter 使用 Go 系统根证书，不提供 `InsecureSkipVerify` 或跳过验证开关。

### 生命周期与本地 Collector

客户端请求结束时、服务端优雅关闭后调用 Runtime `Shutdown`，使用 `-otlp-export-timeout` 为 batch flush 设置上限；同一 Runtime 的重复 Shutdown 幂等。Collector 不可用会增加遥测丢失，但不改写已经完成的 RPC 结果。

Compose 集成通过固定版本 `otel/opentelemetry-collector-contrib:0.115.1` 接收 OTLP/HTTP，使用 batch processor 和 detailed debug exporter。运行：

```bash
make docker-otel-test
```

测试发起真实 etcd 发现 Echo RPC，然后从 Collector 日志确认：

- 客户端 Span 名称为 `rpc.client/rpc.echo.v1.Echo/Echo`。
- 服务端 Span 名称为 `rpc.server/rpc.echo.v1.Echo/Echo`。
- 两个 Span 的 Trace ID 完全相同，服务端 Parent ID 等于客户端 Span ID。
- Resource 分别出现 client/server `service.name`。

手工启动完整环境：

```bash
MINIRPC_OTLP_HTTP_PORT=4318 \
  docker compose -f deploy/docker-compose.yml \
  -f deploy/docker-compose.otel.yml up -d --build

MINIRPC_OTLP_HTTP_PORT=4318 \
  docker compose -f deploy/docker-compose.yml \
  -f deploy/docker-compose.otel.yml --profile client run --rm echo-client
```

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
| `rpc_tls_handshake_failures_total` | Counter | `role` | TLS/mTLS 握手失败并被关闭的连接数 |

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

RPC TLS 和 mTLS 的握手失败通过 `rpc_tls_handshake_failures_total{role="server"}` 计数。证书内容、Common Name、SAN 和 Peer Identity 不作为 Metrics 标签，避免无界基数和敏感信息泄漏；需要排障时结合受控访问的 TLS 错误日志或基础设施审计。

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
- InMemoryExporter 验证 client/server Span 同为一条 Trace，服务端 Parent 指向客户端 Span，失败状态和 attempt 属性正确。
- 零采样比例导出零 Span，但 `trace_flags=00` 和 Traceparent W3C 字符串同步；Shutdown/ForceFlush 幂等且禁用状态为 no-op。
- OTLP Endpoint scheme、insecure 组合、URL 凭据/fragment、Header 名称与值校验通过。
- 每个完成 Span 都产生一条 JSON 日志，失败码和 outcome 分类正确。
- `Debug=false` 不输出开始事件，`Debug=true` 输出开始与完成事件。
- 指标区分成功、失败、超时、取消和限流，Histogram 与重试计数正确。
- 指标标签不包含 `request_id` 或 `trace_id`。
- 512 个路由组合上限和 overflow 聚合生效。
- 并发写入、并发抓取以及关闭观测后的 RPC 路径通过 race detector。
- `/metrics` 的 `GET`、`HEAD` 和非法方法行为通过 HTTP Handler 测试。
- 真实 Echo Server 与 Echo Client 通过日志和 `/metrics` 完成端到端验证。
- 真实 Collector 验证 OTLP batch 到达、service resource 正确且客户端/服务端 Span 共享 Trace ID。

## 已知限制

- Span 和指标保存在单进程内，进程重启后清零，不提供持久化或跨进程聚合。
- 当前只支持 OTLP/HTTP，不包含 OTLP/gRPC、Baggage、Span Events、Metrics/Logs OTLP 或 Collector 端到端持久化。
- Mini-RPC 使用自有 Metadata 传播 Trace ID、Span ID 和采样位；`Traceparent()` 只提供格式转换能力，不解析 W3C Trace Context Header。
- Exporter HTTPS 使用系统根证书，暂不支持自定义根 CA、客户端证书或到 Collector 的 mTLS。
- 采样比例仅控制本地根 Span；服务端继承上游决定，跨多个入口部署时仍需统一采样配置。
- 客户端耗时包含 retry 和退避，当前没有逐 attempt 独立 Histogram。
- `rpc_circuit_state` 一次只导出一个当前状态系列，状态切换后旧状态系列消失。
- RPC 通道的 TLS 参数不会自动应用到 `/metrics`；Metrics HTTP 仍没有 TLS、鉴权、限流或租户隔离，默认仅监听回环地址。
- 日志只保证协议层关联字段稳定，不包含业务请求体、响应体或敏感字段脱敏策略。
- 尚未提供 Grafana Dashboard、Alert Rules 和长期存储配置。
