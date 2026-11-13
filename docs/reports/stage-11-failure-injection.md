# 阶段 11 报告：故障注入与恢复验证

## 阶段

阶段 11：为已完成的注册发现、负载均衡、重试、熔断、限流和可观测调用链接入可重复的故障切换与恢复演练，并记录 attempt、错误率、恢复时间和安全边界。

## 相关提交

- `d47471b feat(resilience): add discovery failover and fault injection drills`
- `fc5cb49 docs(resilience): document failover drills and recovery metrics`
- `f80abc4 fix(server): flush graceful shutdown responses before close`

本报告使用独立文档提交保存测试证据、验收结果和阶段 12 依赖。

## 实现

- 新增 `internal/discovery.Client`，把 Resolver 快照和 Balancer 接入真实 `rpc.ClientConn.Invoke` 调用链。
- 每次 attempt 从最新快照选择实例；同一次调用不会连续选择刚失败的地址。
- 每个实例地址复用独立的有界 Client 和连接池；成功快照移除实例后，发现层关闭并删除对应连接池。
- 跨实例自动故障切换只对 `rpc.WithIdempotent(ctx)` 生效。非幂等调用即使返回临时错误也只执行一次。
- 发现层统一执行有限重试，内部单地址 Client 强制单次 attempt，避免两层重试相乘。
- 本地限流错误保留公开 `RESOURCE_EXHAUSTED` 状态，同时通过 `localRateLimitError` 保留本地限流身份；发现层不会把未发到网络的拒绝切换为实例重试。
- `resolver.Resolver` 在 List/Watch 故障时保留最后可用快照；注册中心恢复后重新 List、重建 Watch，并发布完整的新快照。
- 新增 `testkit.MemoryRegistry`，支持 Register、Deregister、List、Watch、控制面不可用和 Watch 强制断连。
- 新增 `testkit.FaultProxy` 帧级 TCP 代理，支持响应延迟、丢帧、替换和强制断连，不修改业务 Handler。
- `fault-injection` 命令提供表格/JSON 输出、场景筛选、总超时、attempt、failure、error rate 和恢复时间。
- Echo Client 增加 `-etcd-endpoints`、`-retry-attempts`、`-retry-backoff` 和 `-resolver-backoff`；未提供 etcd 参数时继续使用单地址模式。
- Makefile 增加 `make fault-test`，一次执行全部九个确定性场景。
- 补充发现层单元测试：实例切换、全部故障、非幂等单次调用、本地限流不跨实例、注册中心中断与恢复。
- 对已有竞态测试中的极短 wall-clock deadline 增加调度余量，避免 race detector 下请求尚未到达 Server 就被本地 deadline 取消；生产代码的 deadline 语义不变。
- GracefulStop 在活跃请求全部完成后先对 TCP 连接执行半关闭，让最后一个响应以正常数据加 FIN 的顺序到达；连接 worker 退出后再执行完整关闭，超时路径仍会强制关闭。

## 故障场景验收

| 场景 | 预期结果 | 实际结果 |
|---|---|---|
| 单实例切换 | 首次实例失败后切换健康实例 | 2 attempts，选择 `b` 后成功，恢复 7ms |
| 全部实例下线 | 有界失败并返回 `UNAVAILABLE` | 2 attempts，约 2ms 返回可识别错误 |
| 实例重新加入 | 新快照传播后接收请求 | 1 attempt 成功，恢复 2ms |
| 响应错误 | 一次 `UNAVAILABLE` 后幂等重试成功 | 2 attempts，替换 1 个响应，恢复 3ms |
| 响应丢失 | 当前调用 deadline 失败，下一次独立调用恢复 | 丢弃 1 帧，2 次独立 attempt，恢复 202ms |
| 响应延迟 | 超时后迟到响应不破坏下一次调用 | 首次 `DEADLINE_EXCEEDED`，恢复 152ms |
| 强制断连 | 重新建连并重试成功 | 2 attempts、2 个代理连接，恢复 3ms |
| 注册中心抖动 | 保留旧快照，Watch 重连后补全实例 | 2 次 List、2 次 Watch，恢复 5ms |
| 资源耗尽 | 拒绝请求不进入 Handler，无无限重试 | 2 次有界 attempt，释放容量后成功 |

真实一键输出：

```text
SCENARIO             RESULT  ATTEMPTS  FAILURES  ERROR_RATE  RECOVERY  DETAIL
instance-failover    PASS    2         1         50.00%      7ms       attempts=2 selected=b response="failover"
all-instances-down   PASS    2         2         100.00%     2ms       attempts=2 error=rpc: unavailable: dial tcp 127.0.0.1:36045: connect: connection refused elapsed=2ms
instance-recovery    PASS    1         0         0.00%       2ms       instances=1 response="instance-recovered" error=<nil>
response-error       PASS    2         1         50.00%      3ms       attempts=2 replaced=1 response="after-response-error" error=<nil>
dropped-response     PASS    2         1         50.00%      202ms     dropped=1 attempts=2 lost=rpc: deadline_exceeded: request deadline exceeded recovery="after-dropped-response"
delayed-response     PASS    2         1         50.00%      152ms     first=rpc: deadline_exceeded: request deadline exceeded recovery="after-delay" attempts=2
forced-disconnect    PASS    2         1         50.00%      3ms       attempts=2 connections=2 response="after-disconnect" error=<nil>
registry-outage      PASS    0         0         0.00%       5ms       instances=2 list_calls=2 watch_calls=2 snapshot_update=2026-11-13T09:26:56.817670069Z
resource-exhaustion  PASS    2         2         100.00%     2ms       bounded_attempts=2 handler_calls=2 rejected=rpc: resource_exhausted: server concurrency limit exceeded response="after-exhaustion"
```

恢复时间定义为从注入故障到首次健康 RPC 成功；注册中心场景定义为从故障开始到收到健康和完整的新快照。详细定义、错误率公式和治理边界见 [故障注入与恢复验证](../failure-injection.md)。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、单元测试和构建 |
| `make race` | 通过 |
| `golangci-lint run` | 通过 |
| `go test -race -count=3 ./internal/discovery ./internal/testkit ./internal/client ./internal/server` | 通过 |
| `make fault-test` | 通过，九个场景全部 PASS |
| `docker compose -f deploy/docker-compose.yml config --quiet` | 通过 |

关键新增测试：

- `TestDiscoveryFailsOverToHealthyInstance`：停止首个实例后调用第二个实例，并验证移除实例后的连接池清理。
- `TestDiscoveryReturnsBoundedUnavailableWhenAllInstancesFail`：全部下线时 attempt 和耗时都有上限。
- `TestDiscoveryDoesNotFailOverNonIdempotentCall`：非幂等调用只执行一次。
- `TestDiscoveryDoesNotRetryLocalRateLimit`：本地令牌桶拒绝不会在发现层放大为第二次 attempt。
- `TestDiscoveryRetainsSnapshotDuringRegistryOutage`：控制面故障期间保留快照，恢复后重新 Watch 并补全实例。
- `TestFaultProxyReplacesOneResponseAndClientRetries`：替换响应后同调用重试成功。
- `TestFaultProxyDropsOneResponseAndNextCallRecovers`：丢响应表现为当前 deadline 失败，下一次调用恢复。
- `TestFaultProxyClosesConnectionAndNextAttemptRecovers`：强制断连后重新建连。
- `TestFaultProxyDelaysResponseWithoutPermanentFailure`：延迟响应不污染连接和后续调用。

## 验收

- [x] 单实例故障时请求可以切换到其他实例。
- [x] 所有实例故障时客户端返回可识别且有限的 `UNAVAILABLE`。
- [x] 实例重新注册后进入 Resolver 快照并接收请求。
- [x] 注册中心短暂不可用后 List/Watch 可以恢复。
- [x] 延迟、丢响应、响应错误和强制断连均有真实 TCP 故障注入。
- [x] 服务端资源耗尽不会让被拒绝请求进入 Handler，也不会无限重试。
- [x] 重试、熔断、限流和发现选择不会互相形成无限循环。
- [x] 故障场景可通过 `make fault-test` 一条命令启动。
- [x] 每个场景记录预期结果、实际结果、attempt、错误率和恢复时间。
- [x] `docs/failure-injection.md` 明确恢复时间、错误率和跨实例重试边界。
- [x] 实现、设计文档和阶段证据使用独立语义提交。
- [x] GitHub Actions 远端运行成功：[run 43402221808](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43402221808)。

## 已知限制

- 默认演练使用 `MemoryRegistry`，不模拟 Lease TTL 自动过期。
- `FaultProxy` 在完整帧边界注入，不覆盖帧内损坏、乱序、重复帧或真实网络分区。
- 场景按固定顺序执行，没有随机混沌、长时间持续压测或复合故障编排。
- 丢响应不会在同一未完成调用中自动重放，因为客户端无法证明服务端是否已经执行；写操作仍需业务级幂等键。
- 演练结果尚未持久化为 Prometheus 历史指标或长期趋势报告。

## 下一阶段依赖

阶段 12 应复用本阶段的真实 Server/Client、发现模式和故障工具完成：

- 串行、单连接并发、多连接并发、多实例和不同消息体大小的 benchmark。
- 记录 QPS、P50/P95/P99、错误率、连接数、CPU、内存和提交 SHA。
- 对比关闭治理、开启重试/熔断/限流以及不同负载均衡策略的差异。
- 在发布验收中使用 `make fault-test` 防止性能优化破坏故障切换和安全重试边界。
