# 阶段 20 报告：HTTP/2 独立性能验收

## 结论

阶段 20 为 HTTP/2 一元传输建立独立进程性能验收。benchmark 客户端不再在自身进程中创建 HTTP/2 Server，而是通过外部 `-target` 连接由验收脚本启动的独立 Echo Server OS 进程；脚本依次覆盖 h2c、HTTP/2 TLS 和 HTTP/2 mTLS，并为每次测量单独采集服务端进程 CPU 与 RSS。

六种工作负载均为 0 错误率且只保持 1 条 HTTP/2 连接。串行场景验证 HTTP/2 帧封装和 ALPN/握手基线，64 并发场景验证同一连接上的多 stream 复用。连接建立 P50 从 h2c 的 `369us` 增至 HTTP/2 TLS 的 `4.36ms` 和 mTLS 的 `4.64ms`，稳定体现了握手与证书验证成本。

本阶段显著收紧了“客户端与 Server 同进程”的旧边界，但客户端和 Server 仍是同一容器/主机中的两个进程，共享 CPU、内存、内核、调度器和回环网络；结果不构成独立压测机或生产容量承诺。

## 提交

- `036b1b6` `feat(benchmark): add HTTP/2 performance cases`
- `3b1cf4c` `build(benchmark): isolate HTTP/2 client and server processes`
- `d6febbe` `ci(benchmark): gate isolated HTTP/2 performance smoke`
- `f183759` `feat(benchmark): isolate HTTP/2 TLS performance runs`
- `73e6a39` `docs(benchmark): document isolated HTTP/2 performance`

## 实现范围

### Benchmark 场景

- 新增 `h2c`、`h2-tls`、`h2-mtls` benchmark transport。
- 每种 transport 覆盖 `serial/1conn/1024B` 和 `concurrent/1conn/1024B/c64`。
- 工作负载继续通过生成的 `EchoClient` 发出真实 Protobuf 请求，并经过生产 `DialHTTP2`、`ServeHTTP2`、v1 帧编解码、RequestID 校验和 Handler 分发。
- 连接建立表为每个 transport 另取 16 个样本，计时覆盖 `DialHTTP2`、TCP、TLS/mTLS 握手、证书验证、ALPN 和 HTTP/2 初始化；每次 Dial 后执行真实 RPC，确认链路可用。

### 外部 target 与进程隔离

- `-target` 要求 `-transports` 恰好选择一个 HTTP/2 transport，并禁用本地 Server 创建。
- 新增 `-target-ca`、`-target-cert`、`-target-key`、`-target-server-name` 和 `-target-tls-min-version`，复用生产安全加载与严格校验逻辑。
- `h2c` 拒绝 TLS 参数；`h2-mtls` 要求成对客户端证书和私钥；TLS 最低版本默认 1.2。
- 验收脚本为每种 transport 启动独立 Echo Server，使用临时端口和一次性证书，完成 benchmark 后优雅停止 Server，再进入下一 transport。
- 脚本从 `/proc/<server-pid>/stat` 与 `/proc/<server-pid>/statm` 采集 Server CPU 秒数、CPU core 和 RSS；benchmark 客户端资源仍由其自身进程采集。

### 工程入口与 CI

- `make benchmark-http2` 运行完整独立进程验收；`make benchmark-http2-smoke` 使用 150ms 测量窗口验证接线。
- Verify CI 执行 `make benchmark-http2-smoke`，要求三种 transport 均有真实、零失败工作负载，输出包含 `external-http2` topology，并成功采集服务端资源。
- 一次性 CA、Server 证书、Client 身份和私钥只写入 `mktemp -d` 目录，退出 trap 删除；仓库和 CI 不保存证书材料。

## 验收环境

| 项目 | 值 |
|---|---|
| 被测提交 | `f183759a5ff9f982cd721d8ede97cf78b734402a` |
| Go / OS / Arch | `go1.23.12` / `linux` / `amd64` |
| CPU | Intel(R) Xeon(R) Platinum 8352V CPU @ 2.10GHz |
| 可见逻辑 CPU / GOMAXPROCS | 144 / 144 |
| Kernel / Memory | `6.5.0-28-generic` / 251.5 GiB |
| 容器 | 复用本地 `minirpc-test-base`，`CGO_ENABLED=0` |
| 部署拓扑 | benchmark 与 Echo Server 为同一容器内的不同 OS 进程，通过 `127.0.0.1` TCP 通信 |
| 负载参数 | warmup 500ms、measurement 3s、concurrency 64、16 个建连样本/传输、1024B 消息 |

完整命令：

```bash
docker run --rm \
  -v /home/zjs/实习秋招/MiniRPC:/src \
  -w /src \
  -e CGO_ENABLED=0 \
  -e GIT_CONFIG_COUNT=1 \
  -e GIT_CONFIG_KEY_0=safe.directory \
  -e GIT_CONFIG_VALUE_0=/src \
  minirpc-test-base ./scripts/http2-benchmark.sh
```

## 实测结果

### 工作负载

| 场景 | REQ | 错误 | QPS | P50 | P95 | P99 | 连接 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `h2c/serial/1conn/1024B` | 3368 | 0.000% | 1122 | 871us | 982us | 2.33ms | 1 |
| `h2c/concurrent/1conn/1024B/c64` | 17197 | 0.000% | 5708 | 10.99ms | 14.74ms | 16.74ms | 1 |
| `h2-tls/serial/1conn/1024B` | 3251 | 0.000% | 1083 | 910us | 1.03ms | 2.14ms | 1 |
| `h2-tls/concurrent/1conn/1024B/c64` | 19568 | 0.000% | 6500 | 9.53ms | 13.62ms | 15.68ms | 1 |
| `h2-mtls/serial/1conn/1024B` | 3247 | 0.000% | 1082 | 909us | 1.03ms | 2.22ms | 1 |
| `h2-mtls/concurrent/1conn/1024B/c64` | 20285 | 0.000% | 6747 | 9.43ms | 12.97ms | 15.08ms | 1 |

### 连接建立

| Transport | 样本 | 失败 | P50 | P95 | P99 | Max |
|---|---:|---:|---:|---:|---:|---:|
| `h2c` | 16 | 0 | 369us | 555us | 555us | 555us |
| `h2-tls` | 16 | 0 | 4.36ms | 5.67ms | 5.67ms | 5.67ms |
| `h2-mtls` | 16 | 0 | 4.64ms | 5.39ms | 5.39ms | 5.39ms |

### 独立 Server 进程资源

| Transport | 测量窗口 CPU 秒数 | CPU core | RSS |
|---|---:|---:|---:|
| `h2c` | 13.530 | 1.902 | 17,686,528 B |
| `h2-tls` | 15.290 | 2.125 | 18,874,368 B |
| `h2-mtls` | 15.240 | 2.116 | 16,506,880 B |

CPU core 为 Server 进程 CPU 秒数除以 benchmark 墙钟耗时；RSS 为每次 transport 测量结束后的瞬时 resident set，不代表峰值。

## 解读

- `h2c` 建连 P50 为 `369us`，HTTP/2 TLS 为 `4.36ms`；约 4ms 的差值包含 TLS 握手、证书链验证和 ALPN，是本次最稳定、最可解释的成本信号。
- mTLS P50 为 `4.64ms`，相对 TLS 增加约 `280us`；该差额只对应本次短链 ECDSA 测试证书和共享主机，不能当作生产环境 mTLS 固定开销。
- 串行请求 P50 在三种 transport 下为 `871us`、`910us`、`909us`，说明握手后的 1024B 一元调用主要受 Mini-RPC 帧处理、Protobuf、HTTP/2 stream 和调度成本支配。
- 64 并发均保持 1 条连接且零失败，证明多个生成调用可以并发复用同一 `HTTP2Conn`，服务端也按独立 stream 分发。
- 本次 h2-mTLS 并发 QPS 高于 h2c，但这是单轮共享主机测量；QPS 差异不作为 TLS/mTLS 提速结论。

## 本地验证

| 检查 | 结果 |
|---|---|
| `docker run ... minirpc-test-base go test -race ./...` | 通过 |
| `docker run ... minirpc-test-base go vet ./...` | 通过 |
| `docker run ... minirpc-test-base go build ./...` | 通过 |
| `minirpc-test-base ./scripts/http2-benchmark.sh` | 通过；六种工作负载零失败，三种 transport 均采集到独立 Server 资源 |
| `make benchmark-http2-smoke` | 通过；三种 transport、零失败、外部 target 和服务端资源采样均验证 |
| [GitHub Actions CI #43553145826](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43553145826) | 通过；Verify、Lint 和全部 Docker smoke jobs 成功 |

## 验收清单

- [x] h2c、HTTP/2 TLS、HTTP/2 mTLS 均通过外部 target 完成真实生成 RPC。
- [x] 三种 transport 的串行和 64 并发场景均为 0 错误率。
- [x] 64 并发场景均只保持 1 条连接。
- [x] 建连表覆盖 TCP、TLS/mTLS 握手、ALPN 和 HTTP/2 初始化。
- [x] benchmark 与 Echo Server 使用不同 OS 进程。
- [x] 独立 Server 进程 CPU 秒数、CPU core 和 RSS 可采集。
- [x] 一次性证书和私钥只存在于临时目录并在退出时清理。
- [x] CI 加入 `make benchmark-http2-smoke` 门禁。
- [x] [GitHub Actions CI #43553145826](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43553145826) 最终验收通过。

## 已知限制

- 客户端和 Server 虽为独立进程，但同容器、同主机，共享 144 个可见 CPU、内存带宽、内核调度和回环网络。
- 每个场景只运行一次 3s 测量，没有多轮置信区间、离群值分析或长稳测试。
- 只覆盖 1024B 消息、单实例、单连接和最多 64 并发，没有消息大小、连接数、实例数和 stream 优先级矩阵。
- TLS 使用一次性 ECDSA 短链证书和 `localhost` 回环地址，不覆盖生产证书链、OCSP、会话恢复、硬件密钥或跨主机 RTT。
- RSS 是单点采样，不记录峰值、GC 周期或每 Goroutine CPU；Server CPU 也没有进一步拆分协议、Handler 和网络系统调用。
- 吞吐量受共享主机后台任务和客户端/服务端 CPU 竞争影响，不能据此建立生产容量或 SLA。

## 下一阶段依赖

HTTP/2 一元传输现在同时具备功能验收和独立进程性能证据。要进一步形成容量报告，需要在独立压测机或固定 CPU/memory cgroup 下增加多轮重复、消息大小矩阵、连接/实例扩展、TLS 会话恢复和跨主机 RTT。

流式 RPC 和 gRPC 互操作仍不能复用本阶段结论，必须分别定义 stream 生命周期、多消息边界、半关闭、背压、trailers/status 和生成代码接口。
