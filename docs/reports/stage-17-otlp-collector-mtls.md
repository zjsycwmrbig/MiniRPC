# 阶段 17 报告：OTLP Collector 自定义根证书与 mTLS

## 结论

阶段 17 在阶段 16 的 OTLP/HTTP Trace 导出基础上补齐生产 Collector 接入所需的传输身份：Echo Server 和 Echo Client 可以只信任私有 CA、出示客户端证书、显式校验 Collector SAN，并强制最低 TLS 1.2。

本阶段保持 v1 帧协议、公开 RPC API、Span 命名、Trace 传播和采样语义不变。OTLP 导出仍然默认关闭；未配置 `-otlp-endpoint` 时不创建 Exporter。v0.1.0 稳定标签 `v0.1.0` 继续指向 `ffdd515`。

## 提交

- `e011736 feat(tracing): support custom roots and collector mutual TLS`
- `5daab8c feat(cli): configure OTLP collector TLS material`
- `07552f3 build(docker): verify OTLP collector mutual TLS`
- `8c54b6f ci(tracing): gate collector mutual TLS trace export`
- `88b4e03 docs(observability): document secure OTLP collector mTLS`

## 实现范围

### Exporter

- `OTLPExporterConfig` 增加可选 `*tls.Config` 和独立的 `TLSMinVersion`。
- HTTPS Endpoint 始终向 OTLP/HTTP transport 传入明确的 TLS 配置，未设置最低版本时使用 TLS 1.2。
- 自定义 `RootCAs` 替换系统根证书；客户端 `Certificates` 用于 Collector mTLS。
- 调用方配置在发布给 transport 前执行 `Clone()`，避免 exporter 生命周期内修改调用方状态。
- ServerName 未设置时从 OTLP Endpoint host 推导，证书 SAN 必须覆盖 DNS 或 IP 校验目标。
- HTTP Endpoint 携带 TLS 配置、`InsecureSkipVerify=true` 或低于 TLS 1.2 的配置会被拒绝。

### CLI

Echo Server 和 Echo Client 新增：

| 参数 | 行为 |
|---|---|
| `-otlp-ca` | Collector 私有根 CA bundle |
| `-otlp-cert` | Collector mTLS 客户端证书 |
| `-otlp-key` | 与客户端证书匹配的私钥 |
| `-otlp-tls-server-name` | Collector 证书 SAN 校验名称 |
| `-otlp-tls-min-version` | OTLP HTTPS 最低版本，支持 `1.2`、`1.3` |

证书和私钥必须成对设置并完成 PEM、私钥匹配和证书解析。TLS 材料不能脱离 `-otlp-endpoint` 使用。RPC 通道的 `-tls-*` 与 Collector 的 `-otlp-*` 是两套独立配置，不共享自动推导或信任状态。

### Docker 与 CI

- `deploy/otel-collector-tls.yml` 让 OTLP/HTTP Receiver 使用服务端证书、`client_ca_file` 和最低 TLS 1.2。
- `deploy/docker-compose.otel-tls.yml` 让 Collector 与两个 Echo Server/Client 使用独立 CA、客户端证书和 `otel-collector` ServerName。
- `scripts/docker-otel-mtls-smoke.sh` 生成一次性材料，执行真实 etcd 发现 RPC、Collector Span/Trace 关联校验和无客户端证书负向导出。
- `make docker-otel-mtls-test` 是本地与 CI 的统一入口。
- CI 在 Compose 配置阶段验证 TLS overlay，并在 Docker smoke job 执行完整 mTLS drill。

## 安全边界

- OTLP TLS 只适用于 `https://` Endpoint；明文 HTTP 仍要求显式 `-otlp-insecure`，两者不能混用。
- 没有 `InsecureSkipVerify`、跳过 SAN 或接受任意证书的生产开关。
- 私钥、证书内容、CA 路径、Header 值和 Collector 凭据不写入日志、Metrics 或错误文本。
- 测试脚本只在 `mktemp -d` 目录生成私钥，退出时删除；仓库不包含测试证书或密钥。
- Collector 官方镜像以 UID `10001` 运行。测试脚本只在临时测试目录内把服务端私钥调整为该用户可读，客户端私钥仍保持宿主私有；生产必须使用 Secret Manager、read-only mount 和最小文件权限。
- mTLS 证明传输对端身份，不替代 Header/Token 鉴权、Collector 租户隔离或后端访问控制。

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
| `make docker-tls-rotation-test` | 通过：在线证书轮换、退役身份拒绝和失败回退 |
| `make docker-otel-test` | 通过：Collector 收到同 Trace ID 的 client/server Span |
| `make docker-otel-mtls-test` | 通过：私有 CA、客户端身份、Collector 校验和 Trace 关联全部生效 |

mTLS smoke test 最终输出：

```text
Docker OTLP mTLS smoke test passed: custom CA, client identity, collector verification, and trace correlation are enforced.
```

负向导出由 Collector 在 TLS 握手阶段拒绝，关键错误为：

```text
Post "https://otel-collector:4318/v1/traces": remote error: tls: certificate required
```

该错误属于遥测导出失败，不改变已经成功的 Echo RPC 结果，符合“Collector 故障只影响遥测丢失”的边界。

GitHub Actions 在提交 `eac4fd9` 上完成统一验收：[run 43538884658](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43538884658)。`Verify` 的格式、生成代码、vet、单元测试、竞态测试、构建、benchmark smoke 和 Compose 配置检查全部通过；`Lint` 通过；`Docker smoke test` 的明文发现、RPC mTLS、在线证书轮换、OTLP Trace 和 OTLP Collector mTLS 五项全部通过。

## 单元与集成证据

- `httptest.NewTLSServer` 验证自定义 RootCA 可以完成 Trace 导出并命中 `/v1/traces`。
- `testkit.NewTLSMaterial` 验证真实 mTLS 服务端 `RequireAndVerifyClientCert` 下客户端证书生效。
- 配置校验覆盖 HTTP 携带 TLS、`InsecureSkipVerify`、低 TLS 版本和其他既有 Endpoint/Header 边界。
- Compose 测试从 Collector 输出提取 32 位 Trace ID，要求 client/server Span 恰好共享一个 Trace ID。
- Compose 测试检查 client/server `service.name`，随后使用不带客户端证书的 Echo Client 验证 Collector 强制 mTLS。

## 验收清单

- [x] OTLP Exporter 支持自定义根 CA、客户端证书和私钥。
- [x] HTTPS 未提供自定义材料时继续使用系统根证书。
- [x] ServerName 可从 Endpoint host 推导，也可显式覆盖。
- [x] OTLP TLS 最低版本为 1.2，且只接受 1.2/1.3 操作员输入。
- [x] HTTP、`InsecureSkipVerify` 和低版本策略在启动时被拒绝。
- [x] Echo Server/Client 完成 CLI 接线，且与 RPC TLS 参数隔离。
- [x] 真实 Collector mTLS 下完成 RPC、client/server Span 和共享 Trace ID 校验。
- [x] 无客户端证书导出被 Collector 拒绝。
- [x] 临时证书、私钥和 Compose 资源在测试结束后清理。
- [x] 原有格式、vet、单测、竞态、lint、故障、benchmark、明文/TLS/OTLP 门禁未回归。
- [x] GitHub Actions 的 Verify、Lint 和完整 Docker smoke 全部成功。

## 已知限制

- OTLP CA、客户端证书和私钥只在进程启动时加载，尚未接入阶段 15 的原子热轮换；轮换后需要重启导出进程。
- 当前仍只支持 OTLP/HTTP Trace，不包含 OTLP/gRPC、OTLP Logs/Metrics、Baggage 或 Span Events。
- 静态 `-otlp-headers` 仍存在；Collector mTLS 不会替代 Token、租户、Header 或 Collector 后端授权策略。
- Compose 验收使用单节点 Collector 和 debug exporter，不代表生产持久化、高可用、限流或保留策略。
- ServerName 推导依赖 Endpoint host 与证书 SAN 一致；通过 IP 访问但证书仅有 DNS SAN 时必须显式设置匹配的 `-otlp-tls-server-name`。

## 下一阶段依赖

下一步可以将 OTLP TLS 材料纳入独立热加载策略，复用“候选完整验证后原子发布、失败保留 last-known-good”的原则；这需要处理 Exporter/HTTP Transport 的配置生命周期，不能仅修改 CLI 文件路径。

HTTP/2、流式 RPC 和动态策略控制面仍是独立候选批次，不依赖本阶段的 Collector 传输细节，也不得改变 v1 帧协议或既有 Span schema。
