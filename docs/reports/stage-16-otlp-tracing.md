# 阶段 16 报告：OpenTelemetry OTLP Trace 导出

## 结论

阶段 16 在阶段 10 的日志关联和进程内 Span 基础上接入 OpenTelemetry SDK。客户端和服务端现在可以生成标准 Client/Server Span，通过 Mini-RPC Metadata 还原远程父子关系，并以 OTLP/HTTP 批量导出到 Collector。

本阶段保持 v1 帧协议、公开 RPC API、治理调用链和连接池语义不变，v0.1.0 稳定标签仍为 `ffdd515`。OTLP 导出默认关闭；只有显式配置 Endpoint 时才创建 TracerProvider 和 Exporter。

## 提交

- `5eb5a0f build(deps): add OpenTelemetry tracing SDK and OTLP exporter`
- `196d68d feat(tracing): emit correlated OpenTelemetry client and server spans`
- `d0b01ae feat(cli): configure secure OTLP trace export`
- `3a0df6b build(docker): add OpenTelemetry collector trace smoke test`
- `c2bce32 ci(tracing): gate OTLP client and server export`
- `9c8f1e2 docs(observability): document OTLP tracing and sampling`

## 实现范围

- `internal/observability` 持有可选 `sdktrace.TracerProvider`，使用 `ParentBased(TraceIDRatioBased(ratio))` 采样。
- 客户端 Span 名称为 `rpc.client/<service>/<method>`，kind 为 `Client`；服务端 Span 名称为 `rpc.server/<service>/<method>`，kind 为 `Server`。
- 请求 Metadata 传播 `trace_id`、`span_id`、`trace_flags` 和 `request_id`。服务端将上游 ID 还原成 OTel Remote SpanContext，服务端 Parent ID 指向客户端 RPC Span。
- 完成属性包括 `rpc.system`、`rpc.service`、`rpc.method`、`rpc.minirpc.role`、`rpc.minirpc.attempts`、`rpc.minirpc.outcome` 和 `rpc.minirpc.error_code`。
- RPC 失败设置 OTel Error Status，状态消息只包含稳定错误码。
- Resource 包含 `service.name`，支持可选 `service.version` 和 `service.instance.id`；Echo 示例写入稳定服务名和实例 ID。
- Runtime 增加 `ForceFlush` 和幂等 `Shutdown`，Echo 进程退出时在超时预算内刷新 batch。
- `NewOTLPTraceExporter` 只接受带 host 的 `http`/`https` 绝对 Endpoint；HTTP 必须显式 insecure，HTTPS 禁止 insecure，URL 凭据和 fragment 被拒绝。
- 静态 OTLP Header 严格验证名称、值和重复 key；错误只回显 Header 名称，不回显值。HTTPS 使用系统根证书，没有跳过验证开关。

## Span 语义

| 字段 | 客户端 | 服务端 |
|---|---|---|
| 名称 | `rpc.client/<service>/<method>` | `rpc.server/<service>/<method>` |
| Kind | `Client` | `Server` |
| Trace ID | 新建或继承外层 OTel Context | 请求 Metadata |
| Parent ID | 外层 Span；根调用为空 | 客户端 RPC Span |
| 采样 | 根 Span 按比例，子 Span 继承 | 继承客户端 `trace_flags` |

未采样调用仍生成有效 Trace/Span ID，并传播 `trace_flags=00`；它们不进入 Exporter，但 JSON 日志仍记录采样决定后的关联字段。一个逻辑 RPC 对应一条客户端 Span 和一条服务端 Span，retry 次数作为最终 `rpc.minirpc.attempts` 属性，不额外拆分每个 attempt。

## 本地验证

Go 版本：`go1.23.12 linux/amd64`。

| 检查 | 结果 |
|---|---|
| `make check` | 通过：格式、生成代码、vet、单元测试和构建 |
| `make race` | 通过：全部包 `go test -race ./...` |
| `golangci-lint run` | 通过 |
| `make fault-test` | 通过：9 个确定性故障/恢复场景 |
| `make benchmark-smoke` | 通过：明文、TLS、mTLS 全部 0 错误 |
| `make docker-test` | 通过：发现 RPC 和 Prometheus 健康 |
| `make docker-tls-test` | 通过：双实例发现 mTLS 与无证书拒绝 |
| `make docker-tls-rotation-test` | 通过：在线材料轮换、退役身份拒绝和失败回退 |
| `make docker-otel-test` | 通过：Collector 收到同 Trace ID 的 client/server Span |

GitHub Actions 在提交 `1def3b0` 上完成统一验收：[run 43425134200](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43425134200)。`Verify` 的格式、生成代码、vet、单测、竞态、构建、benchmark smoke 和 Compose 配置检查全部通过；`Lint` 通过；`Docker smoke test` 的明文发现、mTLS、在线证书轮换和 OTLP Trace 导出四项全部通过。

单元测试使用 `tracetest.NewInMemoryExporter` 验证：

- 客户端和服务端导出 Span 数量、名称和 kind 正确。
- 两条 Span Trace ID 相同，服务端 Parent ID 等于客户端 Span ID。
- 服务端失败 Status、attempt 和稳定错误码属性正确。
- 零采样比例导出零 Span，但 Metadata 和 Traceparent 同步为 `00`。
- Endpoint scheme、insecure 组合、凭据/fragment 和 Header 校验拒绝非法配置。
- Shutdown 幂等，无 Exporter 或禁用 Runtime 时 flush/shutdown 为 no-op。

## Collector 关联证据

本地 Compose 联调中，Collector debug exporter 实际输出同一 Trace 的客户端和服务端 Span：

```text
Client service.name : mini-rpc-echo-client
Client span         : rpc.client/rpc.echo.v1.Echo/Echo
Client trace/span   : 54c4dc3a4fa2920d1582d494968c558c / 454124bb24f7e3f5

Server service.name : mini-rpc-echo-server
Server span         : rpc.server/rpc.echo.v1.Echo/Echo
Server trace/span   : 54c4dc3a4fa2920d1582d494968c558c / 41745860f6189fa9
Server parent span  : 454124bb24f7e3f5
```

`scripts/docker-otel-smoke.sh` 不只检查 Span 名称和 Resource，还会从 Collector 输出提取全部 32 位 Trace ID，要求客户端/服务端 Span 恰好共享一个 Trace ID。测试通过后自动删除独立 Compose project、容器、网络、卷和本地构建镜像。

## 验收清单

- [x] OTLP 默认关闭，配置 Endpoint 后才启用 OTel Exporter。
- [x] 客户端和服务端 Span 共享 Trace ID，远程 Parent ID 正确。
- [x] 客户端和服务端分别使用 Client/Server kind 与稳定服务 Resource。
- [x] 成功、失败、attempt、outcome 和稳定错误码可观察。
- [x] ParentBased 根采样和 `trace_flags` 传播通过单元测试。
- [x] OTLP Endpoint 和 Header 安全校验通过，凭据不进入日志。
- [x] Echo 进程退出时 flush batch，Shutdown 幂等且有超时。
- [x] 真实 Collector 收到 client/server Span 并通过共享 Trace ID 检查。
- [x] 原有格式、vet、单测、竞态、lint、故障、benchmark、明文和 TLS Docker 门禁未回归。
- [x] GitHub Actions 的 Verify、Lint 和完整 Docker smoke 全部成功。

## 已知限制

- 当前只实现 OTLP/HTTP Trace，不包含 OTLP/gRPC、Baggage、Span Events 或 OTLP Logs/Metrics。
- Exporter HTTPS 使用系统根证书，暂不支持自定义根 CA、客户端证书或到 Collector 的 mTLS。
- 静态认证 Header 由启动参数提供，可能出现在具备本机进程检查权限的操作员视图中；生产部署应通过受控环境注入，并限制进程与容器检查权限。Header 不会写入应用日志或 Metrics。
- 采样比例只作用于本地根 Span，服务端继承上游决定；当前没有集中采样策略、tail sampling 或动态调整。
- 一个逻辑调用只有一个客户端 Span 和一个服务端 Span；重试次数是最终属性，没有逐 attempt 子 Span。
- Collector debug exporter 只用于本地验收，不是生产持久化、鉴权、保留或高可用方案。

## 下一阶段依赖

后续生产化应先补齐 Collector 接入所需的 TLS 根证书、mTLS、认证材料轮换和采样配置来源，再评估 OTLP/gRPC。HTTP/2、流式 RPC 或动态策略控制面可以独立立项，不应改变本阶段的 Span 命名、稳定属性或 v1 帧协议边界。
