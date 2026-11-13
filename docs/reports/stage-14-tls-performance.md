# 阶段 14 报告：TLS 与 mTLS 性能基准

## 阶段

阶段 14：在阶段 13 的 TLS/mTLS 安全闭环上补齐明文、TLS 和 mTLS 的真实 Echo benchmark、连接建立分位采样、JSON/终端输出、CI 短跑和可复现性能证据，不改变 v1 帧协议、公开调用语义或 v0.1.0 稳定基线。

本阶段属于 **v0.2 扩展线**，不移动 `v0.1.0` tag。

## 相关提交

- `0ac5c44 feat(benchmark): add TLS and mutual TLS performance coverage`
- `4936967 build(benchmark): expose TLS smoke benchmark target`
- `7ab5657 ci(benchmark): gate plaintext TLS and mTLS measurements`

本报告和 CI 固定使用独立文档提交。

## 实现

- benchmark 增加 `-transports=plaintext,tls,mtls`，支持选择或去重传输，默认三者全部运行。
- 增加 `-handshakes`，独立采样每种传输的连接建立成本；设置为 `0` 可关闭。
- 新增 TLS 服务端认证和 mTLS 的 1024B 串行、64 并发单连接场景，继续走生产 `EchoClient`、`client.Dial`、TLS Listener、协议编解码和 Handler。
- TLS/mTLS 证书由 `internal/testkit.NewTLSMaterial` 在进程内一次性生成，不写入仓库或磁盘。
- 每个 benchmark 结果增加 transport 字段；终端表格和 JSON 都保留传输归属。
- 建连表记录 plaintext/TLS/mTLS 的样本数、失败数、P50/P95/P99 和 Max。
- 建连计时只覆盖 `client.Dial`；随后执行一个验证 RPC，确认 Server 握手和业务路径可用，该 RPC 不计入建连延迟。
- 新增 `make benchmark-smoke`，以 150ms 测量、20ms 预热、16 并发、32B 串行和每传输 10 个建连样本验证工具与三种真实 RPC 链路。
- GitHub Actions 在 Build 后执行 benchmark smoke，避免 TLS 基准入口、证书生成或传输分支回归。
- benchmark 测试覆盖传输解析、TLS/mTLS 真实 RPC、单连接复用、三种建连采样和分位一致性。

## 性能证据

环境：Go 1.23.12、linux/amd64、Intel Xeon Platinum 8352V、144 个可见逻辑 CPU、GOMAXPROCS 144、Kernel 6.5.0-28-generic、共享主机 `nccl5`。

完整运行使用 commit `7ab56578176c70a3765115fd75bb948088c812e9`，warmup 500ms、measurement 3s、concurrency 64、每传输 64 个建连样本：

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

本机最明确的结论是建连成本：TLS/mTLS P50 相对明文约增加 4ms，且所有建连样本均成功。稳态吞吐差异受同进程客户端/Server 和共享主机调度影响，不能把单次 QPS 高低解释为固定 TLS 开销；尤其是本次 TLS/mTLS 并发 QPS 高于部分明文值，只能说明加密长连接复用路径可运行且错误率为 0。

## 测试

| 命令 | 结果 |
|---|---|
| `gofmt -w benchmark`、`git diff --check` | 通过 |
| `go test -race ./benchmark` | 通过 |
| `make check` | 通过，包含生成一致性、格式、vet、单元测试和构建 |
| `make race` | 通过 |
| `golangci-lint run` | 通过 |
| `make fault-test` | 通过，九个故障场景全部 PASS |
| 明文/TLS Compose 配置渲染 | 通过 |
| `make docker-test` | 通过，明文发现 RPC 与指标 smoke test 成功 |
| `make docker-tls-test` | 通过，mTLS 发现 RPC 与拒绝路径 smoke test 成功 |
| `make benchmark-smoke` | 通过，plaintext/TLS/mTLS 全部 0 错误 |
| `make benchmark` | 通过，完整明文/TLS/mTLS 场景全部 0 错误 |

## 验收

- [x] benchmark 可以选择、组合并去重 plaintext/TLS/mTLS。
- [x] TLS/mTLS 场景通过真实客户端、服务端、证书验证、协议帧和 Echo Handler。
- [x] 串行和 64 并发 TLS/mTLS 场景错误率为 0，并发场景单连接复用。
- [x] 建连采样覆盖明文、TLS、mTLS，并输出样本数、失败数和延迟分位。
- [x] TLS/mTLS 证书为一次性内存材料，未提交私钥或放宽证书验证。
- [x] 终端和 JSON 输出均携带 transport 与连接建立结果。
- [x] 增加 race 测试和 CI short benchmark 门禁。
- [x] README、执行计划、架构、贡献指南、TLS 专题和性能文档已同步。
- [x] GitHub Actions 的 Verify、Race、Build、Benchmark smoke、Lint、明文 Docker smoke 和 mTLS Docker smoke 全部成功：[run 43413424409](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43413424409)。

## 已知限制

- TLS benchmark 使用一次性 ECDSA 短链证书、回环地址和同一进程内 Server，没有覆盖 RSA、会话恢复、吊销检查、组织 CA 或跨主机 RTT。
- 只有单次 3s 共享主机运行，没有多轮置信区间、CPU affinity、独立压测机或长稳尾延迟分析。
- 建连分位包含 TCP Dial 和 TLS/mTLS 握手，不拆分手动 TCP、ClientHello、证书验证和 Finished 阶段。
- benchmark 使用固定 Echo payload，没有覆盖真实业务对象大小分布、代理、丢包或网络抖动。
- CPU/Heap/RSS 是客户端和本地 Server 共享进程的数据，不是独立节点资源拆分。

## 下一阶段依赖

- 证书热加载或轮换需要在阶段 13 的严格验证模型上独立设计，并验证旧连接、新握手、CA 更新和失败回退。
- 若准备发布 v0.2.0，应加入隔离环境多轮 TLS benchmark，并决定是否把证书轮换、OTel 或流式 RPC 纳入版本范围。
- benchmark 趋势比较必须固定 transport、handshake 样本、环境、并发和时长，不能跨不同版本手工选择最好 QPS。
