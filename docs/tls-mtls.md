# RPC TLS 与双向 TLS

v0.2 扩展阶段为 Mini-RPC 的 TCP RPC 通道增加标准库 TLS 和双向 TLS（mTLS）。协议帧、RequestID、连接池、发现、负载均衡和治理语义保持不变，TLS 位于 TCP 字节流与协议编解码之间。

## 安全目标

- 客户端必须验证服务端证书链和服务器名称，不使用 `InsecureSkipVerify`。
- 服务端可以要求并验证客户端证书，未提供或证书不可信的连接不能进入 RPC 解码和 Handler。
- 默认最低版本为 TLS 1.2，可显式提升到 TLS 1.3；配置低于 TLS 1.2 会被拒绝。
- TLS 握手有独立超时，且仍受服务端最大连接数限制。
- 多实例证书可以在同一个 SAN 中覆盖所有已发布的 DNS/IP 地址。

范围仅覆盖 Echo Server 与 Echo Client 之间的 RPC 通道。etcd 客户端、Prometheus HTTP 端点和宿主机端口仍使用当前独立配置，不因 RPC TLS 自动加密。

## 命令行参数

服务端：

| 参数 | 含义 |
|---|---|
| `-tls-cert` | PEM 服务端证书链 |
| `-tls-key` | PEM 服务端私钥，必须与证书同时提供 |
| `-tls-client-ca` | 客户端证书的可信 CA；设置后启用 mTLS |
| `-tls-min-version` | `1.2` 或 `1.3`，默认 `1.2` |
| `-tls-handshake-timeout` | 单次 SYN 后 TLS 握手上限，默认 `10s` |

客户端：

| 参数 | 含义 |
|---|---|
| `-tls-ca` | 验证 RPC 服务端的 PEM CA；为空时使用系统根证书 |
| `-tls-cert` | mTLS 客户端证书 |
| `-tls-key` | mTLS 客户端私钥，必须与证书同时提供 |
| `-tls-server-name` | 固定验证的服务器名称；为空时从每个实例地址推导 |
| `-tls-min-version` | `1.2` 或 `1.3`，默认 `1.2` |

服务端提供证书和私钥后自动只接受 TLS 连接；没有 TLS 参数的现有明文模式保持兼容。

## 本地证书

仓库提供仅用于本地测试的证书生成脚本：

```bash
./scripts/generate-test-certs.sh /tmp/minirpc-tls
```

脚本生成一次性 CA、服务端证书和客户端证书，服务端 SAN 包含：

```text
DNS:localhost, DNS:echo-a, DNS:echo-b, IP:127.0.0.1
```

这些证书只有一天有效期，私钥权限为 `0600`。不要把脚本输出或任何测试私钥提交到 Git。

启动 mTLS Server：

```bash
go run ./cmd/echo-server \
  -addr=127.0.0.1:8443 \
  -metrics-addr=127.0.0.1:9090 \
  -instance-id=secure-echo \
  -tls-cert=/tmp/minirpc-tls/server.crt \
  -tls-key=/tmp/minirpc-tls/server.key \
  -tls-client-ca=/tmp/minirpc-tls/ca.crt
```

发起 mTLS 调用：

```bash
go run ./cmd/echo-client \
  -addr=127.0.0.1:8443 \
  -message="hello over mutual TLS" \
  -tls-ca=/tmp/minirpc-tls/ca.crt \
  -tls-cert=/tmp/minirpc-tls/client.crt \
  -tls-key=/tmp/minirpc-tls/client.key
```

只使用服务端认证时，服务端省略 `-tls-client-ca`，客户端省略证书和私钥。

## 服务发现

服务端注册的是 `host:port`。客户端默认从每个被选实例的 host 部分生成 TLS `ServerName`：

- IP 地址按 IP SAN 校验。
- DNS 名称按 DNS SAN 校验。
- 显式 `-tls-server-name` 会覆盖自动推导，适合所有实例通过同一个稳定服务名访问的证书模型。

证书必须覆盖 Resolver 中实际发布的地址。例如 Compose 使用 `echo-a:8080` 和 `echo-b:8080`，测试证书同时包含 `echo-a`、`echo-b`。生产中不要让服务发现发布一个证书未覆盖的临时容器 IP。

## 实现与指标

- `client.WithTLSConfig` 保存克隆后的配置；每次 Dial 再克隆并补充实例服务器名称，避免并发修改共享 `tls.Config`。
- `server.WithTLSConfig` 将监听器包装为 TLS Listener，并在连接 worker 开始读取 RPC 帧前执行有界握手。
- mTLS 使用 `tls.RequireAndVerifyClientCert` 和独立 `ClientCAs`。
- 握手失败连接会被关闭，不进入业务 Handler；服务端统计增加 `TLSHandshakeFailures`。
- Prometheus 新增 `rpc_tls_handshake_failures_total{role="server"}`，只包含有限 role 标签。
- TLS 握手完成后，连接继续复用现有分帧、并发槽、pending map、超时和取消路径。

可查询：

```promql
sum(rate(rpc_tls_handshake_failures_total{role="server"}[5m]))
```

## 性能与容量证据

完整 benchmark 默认同时运行 plaintext、TLS 和 mTLS，并用 `-handshakes` 单独采样连接建立成本。建连分位只统计 `client.Dial`，TLS/mTLS 模式包含完整握手；采样后的验证 RPC 不计入该延迟。

```bash
make benchmark
make benchmark-smoke
```

阶段 14 的一次同机实测中，明文、TLS、mTLS 建连 P50 分别为 `145us`、`4.19ms`、`4.33ms`；64 并发单连接场景错误率均为 0。共享主机噪声会影响稳态吞吐，不能把单次 QPS 差异解释为固定的 TLS 税。完整环境、结果和限制见[性能基准](benchmarking.md#v02-tls-与-mtls-实测阶段-14)。

## Docker mTLS 验收

```bash
make docker-tls-test
```

该命令会：

1. 生成一次性 CA、服务端证书和客户端证书。
2. 启动 etcd、两个启用 mTLS 的 Echo Server 和 Prometheus。
3. 通过 etcd 发现执行带客户端证书的真实 Echo RPC。
4. 验证没有证书的明文请求失败，并检查 `rpc_tls_handshake_failures_total`。
5. 验证 Prometheus 指标仍在独立 HTTP 端口可抓取。
6. 删除本次 Compose project 和临时 TLS 材料。

基础 Compose 启动方式见 [Docker Compose 发现环境](../README.md#docker-compose-发现环境)。

## 已验证安全边界

- 可信 CA 的 TLS 调用成功。
- 可信 CA 和客户端证书的 mTLS 调用成功，并能在单连接上并发复用。
- 明文客户端无法与 TLS-only Server 完成 RPC。
- mTLS Server 拒绝没有客户端证书的连接。
- 客户端拒绝由不可信 CA 签发的服务端证书。
- TLS 连接可以通过 GracefulStop 正常结束。
- 低于 TLS 1.2 的配置、证书与私钥缺失以及无效 CA 文件会直接失败。

## 已知限制

- 没有证书热加载、自动轮换、ACME 或密钥托管；文件更新后需要重启进程。
- 没有 OCSP、CRL、证书吊销检查和证书透明日志策略。
- 没有按方法或租户区分的客户端证书授权，只验证证书链和用途。
- etcd 连接和 `/metrics` HTTP Server 仍不跟随 RPC TLS 参数自动启用 TLS。
- 默认不覆盖 cipher suite 和 curve 策略，由 Go TLS 安全默认值与最低版本控制。
- 测试证书生成依赖本机 `openssl`；生产证书应由组织 CA 或密钥管理系统签发。
- 当前 TLS benchmark 使用一次性 ECDSA 测试证书和本机回环网络，没有覆盖 RSA、会话恢复、证书链深度、跨主机 RTT 或多轮置信区间。
