# 阶段 13 报告：RPC TLS 与双向 TLS

## 阶段

阶段 13：在 v0.1.0 的 TCP 一元 RPC 基线上增加 TLS 服务端认证、双向 TLS、严格证书配置、握手失败指标和容器化发现验收，保持 v1 帧协议、连接池、负载均衡、治理和错误码兼容。

本阶段属于 **v0.2 扩展线**，不改变已固定的 `v0.1.0` tag。

## 相关提交

- `909da07 feat(security): add TLS and mutual TLS RPC transports`
- `4bfb0b4 build(docker): add mutual TLS discovery smoke environment`
- `f5c59e9 ci(security): verify TLS compose and handshake rejection`
- `216be02 docs(security): document RPC TLS mutual TLS and operations`

本报告和后续 CI 固定使用独立文档提交。

## 实现

- 新增 `internal/security`，集中加载服务端/客户端证书、私钥和 PEM CA。
- 证书与私钥、服务端证书与客户端 CA 的依赖关系在启动前校验，缺失或非法配置直接失败。
- 默认最低版本为 TLS 1.2，可显式选择 TLS 1.3；低于 1.2 的配置被拒绝。
- 客户端保存克隆后的 `tls.Config`，每次 Dial 再克隆并从未显式覆盖的实例地址推导 TLS `ServerName`。
- 多实例可以共享包含多个 DNS/IP SAN 的证书，也可以显式固定统一的 `-tls-server-name`。
- 服务端通过 TLS Listener 接收连接，并在连接 worker 读取 RPC 帧前执行有界握手；握手仍占用服务端最大连接配额。
- 服务端 `-tls-client-ca` 使用 `tls.RequireAndVerifyClientCert`，无证书或不可信证书不能进入协议解码和 Handler。
- TLS 握手失败连接会关闭并计入 `Stats.TLSHandshakeFailures`，同时导出 `rpc_tls_handshake_failures_total{role="server"}`。
- TLS 握手完成后继续复用现有连接池、RequestID pending map、并发槽、deadline、CANCEL、限流和重试路径。
- Echo Client 增加 `-tls-ca`、`-tls-cert`、`-tls-key`、`-tls-server-name` 和 `-tls-min-version`。
- Echo Server 增加 `-tls-cert`、`-tls-key`、`-tls-client-ca`、`-tls-min-version` 和 `-tls-handshake-timeout`。
- 新增一次性 CA/Server/Client 证书生成脚本和 `docker-compose.tls.yml` override。
- 新增 `make docker-tls-test`，通过 etcd 发现执行真实 mTLS Echo RPC，并验证明文拒绝、握手失败指标和 RPC 指标。
- GitHub Actions 同时验证明文 Compose、TLS Compose 和两种 Docker smoke test。

## 安全验收矩阵

| 场景 | 预期结果 | 自动化证据 |
|---|---|---|
| 可信 CA 的 TLS 调用 | 验证证书和服务器名称后成功 | `TestTLSCallUsesVerifiedServerCertificate` |
| mTLS 正常调用 | 客户端证书通过验证，单连接支持 64 并发 | `TestMutualTLSCallAndConcurrentMultiplexing` |
| 明文连接 TLS-only Server | 握手失败，RPC 不能成功 | `TestTLSServerRejectsPlaintextClient` |
| mTLS 缺少客户端证书 | Server 拒绝，失败计数增加 | `TestMutualTLSRejectsClientWithoutCertificate` |
| 不可信服务端 CA | Client 拒绝证书链 | `TestTLSRejectsUntrustedServerCertificate` |
| TLS GracefulStop | 已建立 TLS 连接可以正常关闭 | `TestTLSGracefulStopClosesActiveTransport` |
| 证书/私钥缺失、TLS 1.1 | 启动前配置校验失败 | `TestTLSConfigValidation`、`TestNewWithOptionsValidatesResourceLimits` |
| Docker 双实例发现 + mTLS | Resolver、连接池和 mTLS 全链路成功 | `make docker-tls-test` |
| 容器内明文拒绝 | 明文 RPC 失败且指标可观测 | `make docker-tls-test` |

## 测试

使用 Go 1.23.12、protoc 28.3、golangci-lint v1.64.8、Docker 26.0.1、Compose v2 和 OpenSSL 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、单元测试和构建 |
| `make race` | 通过 |
| `golangci-lint run` | 通过 |
| `go test -race -count=1 ./internal/client ./internal/server ./internal/security ./internal/observability ./internal/testkit` | 通过 |
| `make fault-test` | 通过，九个既有故障场景全部 PASS |
| `docker compose -f deploy/docker-compose.yml config --quiet` | 通过 |
| TLS Compose 配置渲染 | 通过 |
| `make docker-test` | 通过，明文发现 RPC 和指标 smoke test 成功 |
| `make docker-tls-test` | 通过，mTLS 发现 RPC 成功，明文和无证书连接被拒绝 |

最终 mTLS smoke 输出：

```text
Generated TLS test material in /tmp/tmp.uMMuBJPSYT
/tmp/tmp.uMMuBJPSYT/server.crt: OK
/tmp/tmp.uMMuBJPSYT/client.crt: OK
Docker mTLS smoke test passed: discovered RPC required client certificates.
```

## 验收

- [x] Echo Server/Client 可以通过 TLS 完成真实 Protobuf RPC。
- [x] 客户端验证服务端 CA 和实例服务器名称，不使用 `InsecureSkipVerify`。
- [x] 服务端可要求客户端证书并拒绝没有证书的 mTLS 连接。
- [x] TLS 与 mTLS 保持单连接并发多路复用和连接池复用。
- [x] TLS 握手失败连接不会进入协议解码或业务 Handler。
- [x] Prometheus 可以采集握手失败计数，标签不含证书身份。
- [x] TLS 连接支持现有 GracefulStop 和响应排空路径。
- [x] Docker Compose 可通过双实例 etcd 发现执行 mTLS Echo RPC。
- [x] 明文、缺失客户端证书和不可信 CA 均有拒绝测试。
- [x] TLS 测试证书为一次性生成，未提交私钥。
- [x] README、架构、传输、观测、注册中心、贡献指南和专题文档已同步。
- [x] v0.1.0 的协议和默认明文部署保持兼容。

## 已知限制

- 没有证书热加载或自动轮换；证书文件更新后需要重启 Server/Client。
- 没有 OCSP、CRL、吊销检查和证书透明日志策略。
- mTLS 只验证证书链和 clientAuth 用途，没有按服务、方法或租户授权。
- RPC TLS 参数不自动应用到 etcd 客户端或 Prometheus HTTP 端点。
- 未提供自定义 cipher suite、curve 或硬件密钥提供器，依赖 Go 安全默认值和 MinVersion。
- Docker 测试证书使用 OpenSSL 一次性生成，仅适合本地和 CI。
- 尚未测量 TLS 握手成本和加密后请求吞吐，后续性能批次需要加入 TLS benchmark。

## 下一阶段依赖

- v0.2 可以继续选择 OpenTelemetry OTLP Exporter、HTTP/2/流式 RPC 或动态策略控制面；这些能力必须独立设计，不能改变 v1 帧语义。
- 若发布 v0.2.0，应先确定扩展示例集合、完成 TLS benchmark，并验证证书轮换或重启运维流程。
