# 阶段 18 报告：OTLP TLS 材料热加载与轮换

## 结论

阶段 18 将阶段 17 的启动时 OTLP CA、客户端证书和私钥接入既有文件轮询与严格验证机制。Echo Client/Server 可以在不重启进程的情况下生成新的底层 OTLP Exporter，并通过原子替换让后续 batch 使用新的根 CA、客户端身份、ServerName 和 TLS 版本策略。

本阶段保持 v1 帧协议、公开 RPC API、Span schema、采样和客户端/服务端关联语义不变。OTLP 导出仍默认关闭，v0.1.0 稳定标签 `v0.1.0` 继续指向 `ffdd515`。

## 提交

- `87cb0c7` `feat(tracing): hot-reload OTLP exporter TLS material`
- `66174f3` `feat(cli): poll OTLP collector TLS material`
- `c26d8db` `build(docker): verify online OTLP mutual TLS rotation`
- `27c998b` `ci(tracing): gate OTLP collector TLS rotation`
- `21823b1` `docs(observability): document OTLP TLS hot reload`
- `docs(reports): record stage 18 OTLP TLS rotation`

## 实现范围

### Reloadable Exporter

- 新增 `ReloadableOTLPTraceExporter`，对上层 BatchSpanProcessor 保持标准 `sdktrace.SpanExporter` 接口。
- 每次轮换先通过最新 TLS source 创建新的 OTLP/HTTP Exporter；创建失败不修改当前实例。
- 成功候选在写锁内原子替换当前 Exporter，保证已经开始的 `ExportSpans` 完成后才退休旧实例。
- 轮换后仍执行 HTTPS、`InsecureSkipVerify` 和最低 TLS 1.2 校验；没有为热加载放宽安全边界。
- `Shutdown` 等待活动导出并只关闭当前 Exporter；关闭后的 reload 会清理候选并返回错误。

### CLI 与指标

Echo Server 和 Echo Client 新增：

| 参数 | 默认值 | 行为 |
|---|---:|---|
| `-otlp-tls-reload-interval` | `10s` | 轮询 OTLP CA、证书和私钥文件；`0` 关闭轮询 |

OTLP Reloader 与 RPC 通道 `-tls-reload-interval` 完全独立。文件变化后复用 `internal/security.Reloader` 的 PEM、私钥匹配、证书用途、Root CA、ServerName 和最低版本验证。

现有 TLS 材料指标增加固定角色 `otlp`：

- `rpc_tls_reload_total{role="otlp",outcome="success|failure"}`
- `rpc_tls_reload_failures_total{role="otlp"}`
- `rpc_tls_config_version{role="otlp"}`

指标不包含证书主题、文件名、Endpoint 或错误文本。失败日志只输出当前版本，不回显私钥、目录或材料内容。

### Docker 与 CI

- `deploy/docker-compose.otel-tls.yml` 的 Echo Server 使用 `200ms` 测试轮询周期。
- 新增 `scripts/docker-otel-mtls-rotation-smoke.sh` 与 `make docker-otel-mtls-rotation-test`。
- 脚本生成两组独立 CA、服务端证书和客户端证书，在 Echo Server 持续运行时替换挂载目录中的材料。
- 脚本要求 Echo Server 输出 OTLP 材料版本更新，并让 Collector 使用同一组新服务端材料重启。
- 轮换后必须出现新的 client/server 共享 Trace ID；轮换前基线 Trace 仍保留。
- 脚本保留第一组客户端身份，使用第二组根 CA 连接新 Collector，验证退役身份被拒绝。
- 无客户端证书的负向验收以 Collector 端 TLS 拒绝日志为准；客户端可能报告远程 TLS alert 或连接中断后的 transport 错误，两种时序都不削弱服务端拒绝证据。
- CI Docker smoke job 增加到六项：明文发现、RPC mTLS、RPC TLS 轮换、OTLP Trace、OTLP mTLS、OTLP mTLS 轮换。

## 轮换时序

```text
文件内容变化
  -> security.Reloader 读取完整材料集合
  -> PEM / 私钥 / 用途 / CA / TLS 版本验证
  -> 发布原子 TLS snapshot
  -> Reloadable Exporter 构建新 OTLP client
  -> 等待正在执行的 ExportSpans 完成
  -> 原子切换 current exporter
  -> 后续 batch 使用新 CA 与客户端身份
```

任何验证或构建失败都停留在“当前 Exporter 继续工作”，不会先关闭旧 Exporter 再尝试新配置。安全材料切换与 RPC 调用链解耦，遥测失败不改变业务调用结果。

## 本地验证

环境：`go1.23.12 linux/amd64`，Docker Compose v2。

| 检查 | 结果 |
|---|---|
| `go test -race ./internal/observability ./cmd/echo-client ./cmd/echo-server` | 通过 |
| `make check` | 通过：格式、生成代码、vet、单元测试和构建 |
| `make race` | 通过：全部包 `go test -race ./...` |
| `golangci-lint run` | 通过 |
| `make fault-test` | 通过：9 个确定性故障/恢复场景 |
| `make benchmark-smoke` | 通过：明文、TLS、mTLS 全部 0 错误 |
| `make docker-test` | 通过：发现 RPC 和 Prometheus 健康 |
| `make docker-tls-test` | 通过：RPC mTLS 与无证书拒绝 |
| `make docker-tls-rotation-test` | 通过：RPC TLS 在线轮换和失败回退 |
| `make docker-otel-test` | 通过：Collector 收到共享 Trace ID 的 client/server Span |
| `make docker-otel-mtls-test` | 通过：OTLP 私有 CA、mTLS 和 Trace 关联 |
| `make docker-otel-mtls-rotation-test` | 通过：在线 OTLP 材料轮换、新 Trace 关联和退役身份拒绝 |

阶段目标最终输出：

```text
Docker OTLP mTLS rotation smoke test passed: live exporters adopted new roots and identity, post-rotation traces correlated, and retired credentials were rejected.
```

本地完整轮换执行中，基线 Trace 为 `71beafafc83458b5a6d69aa52c7b218f`，轮换后新 Trace 为 `c80001a2836627c7e13b00dab87ba4d9`。两个 Trace 均包含 client/server Span；使用退役客户端材料时，Collector 在 TLS 握手阶段拒绝导出。

## 单元测试证据

- 同一监听地址的 Collector 在测试中切换服务端证书和 client CA，客户端同时切换 Root CA 与客户端证书。
- Reloadable Exporter 重建后，Collector Handler 观察到新的客户端证书 Issuer，证明新 Exporter 使用了新材料。
- 将 TLS source 替换为 `InsecureSkipVerify` 后 Reload 失败，下一次导出仍使用上一份有效客户端身份。
- `go test -race` 覆盖 exporter 并发导出、原子替换、旧实例退休和 Runtime Shutdown 路径。
- Metrics 单测验证 `role="otlp"` 的成功计数和配置版本渲染。

## 验收清单

- [x] OTLP CA、客户端证书和私钥支持文件轮询。
- [x] 新 Exporter 构建成功后才替换当前实例。
- [x] 无效 TLS source 不替换 last-known-good Exporter。
- [x] 正在进行的导出完成后才退休旧 Exporter。
- [x] Echo Server/Client CLI 与 Compose 完成接线，轮询周期可与 RPC TLS 独立设置。
- [x] OTLP 材料升级继续执行 HTTPS-only、禁止 skip verify 和 TLS 1.2 下限。
- [x] 真实容器中 Echo Server 不重启即采用新的 CA 和客户端身份。
- [x] 轮换后 client/server Span 在新的共享 Trace ID 下导出。
- [x] 退役客户端身份被 Collector 拒绝。
- [x] 临时证书、私钥和 Compose 资源在测试结束后清理。
- [x] 原有格式、vet、单测、竞态、lint、故障、benchmark 和全部容器门禁未回归。
- [ ] GitHub Actions 最终验收结果（由后续 CI 确认提交补充）。

## 已知限制

- 一次性同时切换到互不信任的 CA 会造成轮换窗口内遥测丢失；生产滚动更新必须准备新旧 CA 共存的重叠窗口。
- 成功后重建 Exporter 会产生新的 HTTP/TLS 连接；旧连接在旧 Exporter 退休时关闭。当前不承诺连接级的无缝复用。
- Collector 自身的服务端证书热加载、监听器平滑重启或高可用不在本阶段范围内；Compose 验收通过重启 Collector 加载新服务端材料。
- OTLP TLS 热加载不覆盖静态 Header/Token 轮换。Header 仍由启动参数提供，可能需要独立的凭据控制面。
- 当前仍只实现 OTLP/HTTP Trace，不包含 OTLP/gRPC、Baggage、Span Events 或 OTLP Logs/Metrics。
- mTLS 证明传输身份，不替代 Collector 租户隔离、鉴权、采样策略或后端访问控制。

## 下一阶段依赖

OTLP TLS 材料生命周期已闭环到“校验、发布、Exporter 替换、失败回退和容器轮换”。后续可独立推进 HTTP/2 传输、流式 RPC 或动态策略控制面；这些能力不依赖 Collector 证书文件，也不得改变 v1 帧协议和既有 Span schema。
