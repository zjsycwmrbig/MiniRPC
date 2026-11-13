# 性能基准

本文记录 Mini-RPC 的基准方法、指标定义、可复现命令以及 v0.1.0/v0.2 实测结果。benchmark 的目标是验证真实调用链的延迟组成、并发复用、实例分布、治理开销和 TLS/mTLS 成本，而不是给出脱离硬件与部署拓扑的生产容量承诺。

## 基准入口

```bash
make benchmark
```

等价编译和运行方式：

```bash
CGO_ENABLED=0 go run -trimpath ./benchmark
```

机器可读输出：

```bash
CGO_ENABLED=0 go run -trimpath ./benchmark -json
```

可调参数：

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-warmup` | `500ms` | 每个场景进入测量前的预热时间 |
| `-duration` | `3s` | 每个场景的测量时间 |
| `-concurrency` | `64` | 并发工作协程数 |
| `-sizes` | `32,256,1024,4096` | 串行场景的 Echo 消息字节数 |
| `-transports` | `plaintext,tls,mtls` | 选择需要运行的传输；可去重并任意排序 |
| `-handshakes` | `64` | 每种传输的连接建立样本数；`0` 表示关闭采样 |
| `-json` | `false` | 输出完整 JSON，而不是终端表格 |

工具本身的功能检查：

```bash
go test -race ./benchmark
CGO_ENABLED=0 go run -trimpath ./benchmark \
  -duration=150ms -warmup=20ms -concurrency=16 -sizes=32 -handshakes=10
```

CI 使用等价的 `make benchmark-smoke`，它只验证真实明文/TLS/mTLS RPC、采集和输出链路，不对共享 Runner 的吞吐量设置阈值。

短跑只用于检查采集、输出和失败检测；发布结论不得使用短跑结果。

## 场景

| 场景 | 连接 | 并发 | 实例 | 治理 | 目的 |
|---|---:|---:|---:|---|---|
| `serial/1conn/{32,256,1024,4096}B` | 1 | 1 | 1 | 关闭 | 观察消息体大小对串行调用成本的影响 |
| `concurrent/1conn/1024B/c64` | 1 | 64 | 1 | 关闭 | 验证单连接多路复用和 RequestID 并发关联 |
| `concurrent/8conn/1024B/c64` | 8 | 64 | 1 | 关闭 | 验证有界连接池扩展与每连接并发上限 |
| `multi-instance/round-robin/1024B/c64` | 2 | 64 | 2 | 发现重试 | 验证 Resolver、Round Robin 和按地址连接池 |
| `multi-instance/weighted-1-2/1024B/c64` | 2 | 64 | 2 | 发现重试 | 验证加权 Round Robin 的 1:2 分流量 |
| `governance-off/serial/1conn/1024B` | 1 | 1 | 1 | 关闭 | 治理开关对照基线 |
| `governance-on/serial/1conn/1024B` | 1 | 1 | 1 | 重试、熔断、令牌桶准入 | 观察同场景治理准入成本 |
| `tls/serial/1conn/1024B` | 1 | 1 | 1 | 关闭 | TLS 服务端认证下的串行调用 |
| `tls/concurrent/1conn/1024B/c64` | 1 | 64 | 1 | 关闭 | TLS 下的单连接并发多路复用 |
| `mtls/serial/1conn/1024B` | 1 | 1 | 1 | 关闭 | 双向 TLS 下的串行调用 |
| `mtls/concurrent/1conn/1024B/c64` | 1 | 64 | 1 | 关闭 | mTLS 下的单连接并发多路复用 |

所有场景通过生成的 `EchoClient` 发出真实 Protobuf 请求，并使用真实 TCP Server/Client。双实例场景使用 `MemoryRegistry`、Resolver 快照、发现客户端和对应负载均衡器，没有绕过网络调用链。

TLS 与 mTLS 场景在内存中生成一次性 CA、服务端和客户端证书，再走生产 `server.WithTLSConfig`、`client.WithTLSConfig`、TLS Listener 和握手路径，不提交证书或私钥。连接建立表另取每种传输的独立样本：计时窗口覆盖 `client.Dial`，TLS/mTLS 因此包含完整握手；计时结束后执行一个验证 RPC，确保 Server 已完成握手且链路真实可用，该 RPC 不计入建连延迟。

治理开启场景使用三次重试上限、1ms 初始退避、熔断阈值 5 和高速率令牌桶。正常请求不会触发限流拒绝，因此对比覆盖的是准入、状态检查和令牌获取路径，不是限流饱和后的拒绝路径。

## 指标定义

- **REQ**：测量窗口内发起的全部 Echo 请求数，包含失败请求。
- **ERROR**：`Failures / Requests`。响应错误、超时、取消或回显内容不一致均计为失败。
- **QPS**：`Requests / Elapsed`，不是只统计成功请求。
- **P50/P95/P99/Max**：对测量窗口内全部请求延迟使用 nearest-rank 计算；JSON 同时保留 Max。
- **CONNS**：测量结束时客户端当前活跃连接数。
- **CPU_CORES**：`/proc/self/schedstat` 在测量窗口内的进程 CPU 秒数除以墙钟耗时。
- **HEAP**：测量结束时的 Go `runtime.MemStats.HeapAlloc`。
- **RSS**：测量结束时的 `/proc/self/statm` resident set size。
- **DISTRIBUTION**：双实例场景中两个服务端 Handler 实际收到的请求数，能反映重试后的真实落点。
- **连接建立 P50/P95/P99/Max**：每次新建客户端连接从 `client.Dial` 开始到返回为止；plaintext 包含 TCP 建连，TLS/mTLS 额外包含握手和证书验证。

CPU、Heap 和 RSS 来自同一个 benchmark 进程。由于客户端和两个本地测试 Server 都在该进程中，它们不是独立客户端或独立生产节点的资源拆分。RSS 是瞬时值，会受 Go 内存回收、页回收和共享库映射影响；不要把它解释为峰值内存。

## v0.2 TLS 与 mTLS 实测（阶段 14）

### 环境

| 项目 | 值 |
|---|---|
| Commit | `7ab56578176c70a3765115fd75bb948088c812e9` |
| Go / OS / Arch | `go1.23.12` / `linux` / `amd64` |
| CPU | Intel(R) Xeon(R) Platinum 8352V CPU @ 2.10GHz |
| 可见逻辑 CPU / GOMAXPROCS | 144 / 144 |
| Kernel / 主机 | `6.5.0-28-generic` / `nccl5` |
| Memory | 251.5 GiB |
| 负载参数 | warmup 500ms、measurement 3s、concurrency 64、64 个建连样本/传输 |

### 结果

```text
CASE                                   REQ    ERROR   QPS    P50     P95      P99      CONNS  CPU_CORES  HEAP    RSS
concurrent/1conn/1024B/c64             48734  0.000%  16229  3.36ms  7.25ms   8.19ms   1      0.255      7.8MiB  9.0MiB
concurrent/8conn/1024B/c64             52272  0.000%  17418  2.73ms  10.42ms  15.63ms  8      0.231      6.9MiB  11.9MiB
governance-off/serial/1conn/1024B      14729  0.000%  4910   150us   448us    647us    1      0.000      6.9MiB  9.0MiB
tls/serial/1conn/1024B                 12776  0.000%  4258   161us   484us    641us    1      0.000      4.1MiB  9.0MiB
tls/concurrent/1conn/1024B/c64         58571  0.000%  19505  2.79ms  7.34ms   8.41ms   1      0.007      8.7MiB  9.0MiB
mtls/serial/1conn/1024B                13645  0.000%  4548   154us   476us    620us    1      0.048      5.6MiB  9.0MiB
mtls/concurrent/1conn/1024B/c64        60334  0.000%  20100  2.75ms  7.05ms   8.05ms   1      0.275      9.8MiB  9.0MiB

TRANSPORT  SAMPLES  FAILURES  P50     P95     P99     MAX
plaintext  64       0         145us   210us   245us   245us
tls        64       0         4.19ms  4.77ms  8.07ms  8.07ms
mtls       64       0         4.33ms  4.77ms  8.60ms  8.60ms
```

完整运行还包含 32B/256B/1024B/4096B 明文串行、双实例 Round Robin/加权和治理开关结果；这些历史场景仍保持 0 错误率，Round Robin 为 `36152:36152`，加权 1:2 为 `20951:41904`。

### 解读与边界

- 建连成本是本阶段最稳定的 TLS 信号：本机明文 P50 为 `145us`，TLS 为 `4.19ms`，mTLS 为 `4.33ms`。TLS 和 mTLS 的差额落在共享主机噪声范围内，不能据此推导证书验证的固定成本。
- TLS/mTLS 工作负载复用既有长连接，加密后的 64 并发场景仍为 0 错误且每场景只保持 1 条连接，证明握手后的请求继续走原有 pending map 和并发槽。
- 稳态 QPS 受同进程客户端/Server、后台任务和共享主机调度影响；本次 TLS 并发值高于部分明文值，不能解释为“TLS 更快”。发布容量结论需要独立进程、多轮测量和固定资源配额。
- 测试证书在本进程内生成并使用回环地址，证书链较短，不包含生产组织 CA、吊销查询、跨机房 RTT 或硬件密钥。

## v0.1.0 发布实测

### 环境

| 项目 | 值 |
|---|---|
| Commit | `7b94338f925379e75e1892774bcf54ab14e9297b` |
| Go | `go1.23.12` |
| OS / Arch | `linux/amd64` |
| CPU | Intel(R) Xeon(R) Platinum 8352V CPU @ 2.10GHz |
| 可见逻辑 CPU | 144 |
| GOMAXPROCS | 144 |
| Kernel | `6.5.0-28-generic` |
| 主机 | `nccl5` |
| Memory | 251.5 GiB |
| 编译 | `CGO_ENABLED=0 go run -trimpath ./benchmark` |
| 负载参数 | warmup 500ms、measurement 3s、concurrency 64 |

### 结果

以下是一次完整默认运行，不是最佳值筛选：

```text
Mini-RPC benchmark
Commit: 7b94338f925379e75e1892774bcf54ab14e9297b
Go: go1.23.12  OS: linux/amd64  CPUs: 144  GOMAXPROCS: 144
CPU: Intel(R) Xeon(R) Platinum 8352V CPU @ 2.10GHz
Kernel: 6.5.0-28-generic  Host: nccl5  Memory: 251.5 GiB

CASE                                   REQ    ERROR   QPS    P50     P95     P99      CONNS  CPU_CORES  HEAP    RSS      DISTRIBUTION
serial/1conn/32B                       26224  0.000%  8741   106us   138us   227us    1      0.057      4.3MiB  11.2MiB
serial/1conn/256B                      25299  0.000%  8433   109us   151us   253us    1      0.064      4.4MiB  8.9MiB
serial/1conn/1024B                     19316  0.000%  6439   128us   343us   512us    1      0.121      4.2MiB  12.3MiB
serial/1conn/4096B                     11963  0.000%  3988   164us   479us   1.21ms   1      0.053      2.9MiB  11.2MiB
concurrent/1conn/1024B/c64             63543  0.000%  21167  2.59ms  5.26ms  5.95ms   1      0.238      8.0MiB  7.9MiB
concurrent/8conn/1024B/c64             70350  0.000%  23420  2.08ms  8.09ms  11.93ms  8      0.231      6.7MiB  13.0MiB
multi-instance/round-robin/1024B/c64   81819  0.000%  27240  1.78ms  6.20ms  7.82ms   2      0.287      7.2MiB  16.2MiB  a=40909,b=40910
multi-instance/weighted-1-2/1024B/c64  62402  0.000%  20782  3.05ms  8.34ms  9.18ms   2      0.284      7.7MiB  18.3MiB  a=20801,b=41601
governance-off/serial/1conn/1024B      16639  0.000%  5546   132us   425us   585us    1      0.064      4.2MiB  14.8MiB
governance-on/serial/1conn/1024B       16129  0.000%  5376   131us   436us   584us    1      0.041      4.2MiB  12.5MiB
```

### 解读与边界

- 所有场景错误率均为 0，连接数符合配置，Round Robin 为 40909/40910，加权 1:2 为 20801/41601，说明真实调用、并发关联和实例选择都在工作。
- 32B 与 256B 串行结果接近，说明小消息主要受协议、调度和系统调用开销支配；4096B 的 P99 上升到 1.21ms，体现更大的编解码与 I/O 成本。
- 64 并发下，8 连接场景本批次观察到 23420 QPS，单连接场景为 21167 QPS；两者差距会随调度和共享主机负载波动，不能据此断言连接池固定带来某个比例收益。
- 双实例发现路径包含额外 Resolver、选择、按地址连接池和治理开销；本批次 Round Robin 为 27240 QPS，加权 1:2 为 20782 QPS，绝对值继续受共享主机噪声影响。
- 治理关闭为 5546 QPS，开启为 5376 QPS；P50 分别为 132us 和 131us。该单次差异落在本地短测噪声范围内，只能证明正常治理路径可运行，不能外推为生产 SLA。
- 这是共享 144 vCPU 主机上的单次 3s 测量，客户端与本地服务端争夺同一进程 CPU。正式容量评估应在隔离客户端/服务端、固定 CPU 配额、延长测量时间和多轮重复的条件下完成。

## 可复现要求

发布或比较性能时至少保留：

1. commit SHA 与工作区是否干净。
2. Go 版本、`GOOS/GOARCH`、`GOMAXPROCS`、CPU 型号和逻辑核数。
3. Kernel、主机类型、客户端与服务端部署拓扑。
4. transport 列表、warmup、measurement、消息体、并发、连接、实例和建连样本数量。
5. QPS、错误率、请求/建连 P50/P95/P99、CPU、Heap 和 RSS。
6. 原始 JSON 或完整终端输出，避免仅记录手工挑选的摘要。

## 已知限制

- 不使用独立压测机、独立服务端进程或固定 CPU affinity。
- 默认测量时间仅 3s，没有统计置信区间、多轮分位数或尾延迟长稳测试。
- 消息由同一 payload 重复发送，TLS benchmark 使用一次性 ECDSA 短链证书和回环地址；未覆盖 RSA、会话恢复、生产证书链、代理或跨机房 RTT。
- 双实例通过内存注册中心运行，不测量 etcd Watch 更新频率、Lease 压力和发现缓存失效。
- CPU 使用进程级 `/proc/self/schedstat`，没有拆分客户端、Server 和各 Goroutine 的热点。
- `go run` 包含构建启动开销，但耗时统计只覆盖 warmup 后的测量窗口。
