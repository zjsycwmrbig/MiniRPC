# 阶段 12 报告：性能基准与 v0.1.0 发布工程

## 阶段

阶段 12：补齐端到端 benchmark、客户端容器镜像、一键 Compose smoke test、贡献与发布文档，并完成 v0.1.0 本地验收和远端固定。

## 相关提交

- `c0d2c22 perf(benchmark): add end-to-end load and resource benchmarks`
- `f200be8 build(docker): add client image and compose smoke test`
- `b7ce3ae fix(benchmark): preserve CPU utilization precision`
- `7b94338 fix(benchmark): expose sub-hundredth CPU utilization`

性能报告对应 `7b94338f925379e75e1892774bcf54ab14e9297b`；发布文档、CI 固定和 annotated tag 在其后的独立提交中完成。

## 实现

- 新增 `benchmark` 命令，复用真实 Echo Protobuf、TCP Server/Client、连接池、发现客户端、Resolver 和负载均衡器。
- 覆盖 32B、256B、1024B 和 4096B 串行调用，以及 64 并发下的单连接、8 连接、双实例 Round Robin、双实例 1:2 加权 Round Robin。
- 增加治理关闭/开启对照，覆盖重试、熔断和令牌桶的正常准入路径。
- 输出 QPS、错误率、P50/P95/P99、最大延迟、请求数、连接数、实例分布、进程 CPU Cores、Heap、RSS、Go 版本、机器环境和 Git SHA。
- 支持终端表格与 `-json` 两种输出，JSON 保留 CPU 秒数、纳秒延迟和 BuildInfo，供后续趋势分析使用。
- CPU 使用 `/proc/self/schedstat` 计算进程级 CPU 时间；Heap 使用 `runtime.MemStats`，RSS 使用 `/proc/self/statm`。
- 每次测量前执行 warmup 和 `runtime.GC()`；任一 warmup 失败即终止场景，避免带着错误状态进入正式统计。
- Dockerfile 拆分为 `server-build`、`client-build`、`server` 和 `client` 目标，静态编译并裁剪符号与调试信息。
- Compose 增加 Echo healthcheck、Prometheus 健康依赖、profiled `echo-client`、回环地址端口绑定和可覆盖端口变量。
- 新增 `scripts/docker-smoke.sh` 和 `make docker-test`：启动 etcd、两个 Echo Server、Prometheus，通过 etcd 发现执行真实 RPC，验证响应与 `rpc_requests_total`，并在退出时清理资源。
- 新增 `CHANGELOG.md`、`CONTRIBUTING.md`、`docs/benchmarking.md` 和本报告；README 固定 v0.1.0 当前状态并提供发布入口。
- GitHub Actions 增加 Docker smoke job，使代码格式、生成一致性、vet、测试、竞态、构建、lint 和容器发现调用共同成为远端门禁。

## 性能证据

完整运行：

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

Round Robin 的实际 Handler 分布为 `40909:40910`，加权 1:2 为 `20801:41601`，全部场景错误率为 0。CPU、Heap 和 RSS 是客户端与本地测试 Server 共享 benchmark 进程的合计/瞬时指标，不是独立生产节点资源。完整方法、指标定义和限制见[性能基准](../benchmarking.md)。

## 测试

使用 Go 1.23.12、protoc 28.3、golangci-lint v1.64.8、Docker 26.0.1 与 Compose v2 验收。

| 命令 | 结果 |
|---|---|
| `gofmt -w benchmark`、`git diff --check` | 通过 |
| `make check` | 通过，包含生成一致性、格式、vet、单元测试和构建 |
| `make race` | 通过 |
| `golangci-lint run` | 通过 |
| `go test -race ./benchmark` | 通过 |
| `make fault-test` | 通过，九个故障场景全部 PASS |
| `docker compose -f deploy/docker-compose.yml config --quiet` | 通过 |
| `make docker-test` | 通过，真实发现 RPC 与两项指标 smoke check 成功 |
| `make benchmark` | 通过，完整运行十个场景，发布结果错误率为 0 |

Docker smoke 实际结果：

```text
Docker smoke test passed: discovery RPC and Prometheus metrics are healthy.
```

故障回归仍通过实例切换、全部下线、实例恢复、响应错误、丢响应、延迟响应、强制断连、注册中心抖动和资源耗尽九个场景。性能与工程化提交没有改变既有治理和恢复语义。

## 验收

- [x] 提供串行、单连接并发、连接池并发、多实例和不同消息大小 benchmark。
- [x] benchmark 输出 QPS、P50/P95/P99、错误率、连接数和实例分布。
- [x] benchmark 输出 CPU、Heap、RSS、环境、编译信息和提交 SHA。
- [x] 提供治理关闭/开启和 Round Robin/加权 1:2 对比。
- [x] Docker Compose 可启动 etcd、两个 Echo Server、Prometheus 和按 profile 启动的 Echo Client。
- [x] 容器 smoke test 执行真实服务发现 RPC 并验证服务端指标。
- [x] Docker Compose 端口默认只绑定回环地址，测试端口可覆盖以避免宿主机冲突。
- [x] `make check`、race、lint、fault-test、benchmark 和 Docker smoke 全部通过。
- [x] README、贡献指南、变更记录、专题文档和阶段报告形成可发布文档集。
- [x] 13 个计划批次全部完成，v0.1.0 功能、故障恢复和性能证据闭环。
- [x] GitHub Actions 的 Verify、Race、Build、Lint 和 Docker smoke job 全部成功：[run 43406014692](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43406014692)。

## 已知限制

- 发布 benchmark 是共享主机上的单次 3s 测量，没有隔离客户端/服务端，也没有多轮置信区间或长稳尾延迟分析。
- 双实例使用内存注册中心，尚未测量 etcd Watch 压力、跨主机网络和独立进程资源拆分。
- benchmark 只覆盖固定 Echo payload，不覆盖 TLS、代理、真实业务对象分布或流式传输。
- Compose 用于本地 smoke test，镜像标签可读但未固定 digest；容器也未完成非 root、只读文件系统或资源配额加固。
- Docker smoke 依赖宿主机运行中的 Docker daemon，不能在无容器运行时的最小环境中执行。
- Prometheus 只验证抓取与文本指标，未附带 Grafana Dashboard、告警规则或长期存储。

## 后续依赖

v0.1.0 的实现批次到此结束。后续扩展应继续保留本阶段的回归门禁：

- 性能优化前后使用相同 commit 信息、环境记录和完整场景，禁止只比较手工筛选的最好数字。
- Docker 或公开 API 改动必须同时运行真实发现 RPC smoke test 和完整 race/fault 测试。
- 引入流式 RPC、TLS、多语言生成或生产认证时，需要新增协议版本和独立兼容性验收，不能悄悄改变 v1 帧语义。
- 若将 benchmark 用于容量规划，应迁移到隔离压测环境并增加多轮重复、长稳、内存峰值和网络场景。
