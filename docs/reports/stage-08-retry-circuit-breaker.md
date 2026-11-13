# 阶段 8 报告：重试和熔断

## 阶段

阶段 8：为显式幂等的调用实现有限重试、指数退避、随机抖动和三态熔断。

## 相关提交

- `1e7999f feat(governance): add bounded retries and circuit breaking`
- `8190e3c docs(governance): document retry safety and breaker transitions`

本报告使用独立文档提交记录上述实现和说明基线，便于阶段 9 复用稳定的错误、重试和快速失败语义。

## 实现

- 新增 `governance.RetryPolicy`，支持最大 attempt 数、基础退避、退避上限、总重试耗时预算和抖动函数。
- 默认策略为 3 次 attempt、50ms 初始退避、1s 最大退避、2s 重试总预算和 0–50% 随机抖动。
- 仅对 `UNAVAILABLE`、`RESOURCE_EXHAUSTED` 和调用总 deadline 仍有效的 `DEADLINE_EXCEEDED` 开放重试。
- `INVALID_ARGUMENT`、`NOT_FOUND`、`INTERNAL`、`CANCELED` 和 `PROTOCOL_ERROR` 不自动重试。
- 所有 attempt 共享进入重试前确定的 Context deadline，不会为单次重试重新获得完整超时。
- 每次请求写入从 0 开始的 `retry_attempt` Metadata，供服务端和后续可观测性使用。
- 在 `pkg/rpc` 增加 `WithIdempotent` 和 `IsIdempotent`，只有显式声明安全的调用才允许重试。
- 新增并发安全的 `CircuitBreaker`，实现 Closed、Open、HalfOpen 和单探测请求约束。
- 默认连续失败阈值为 5，Open 冷却时间为 30 秒；探测失败立即回到 Open，探测成功恢复 Closed。
- 熔断打开前已发出的旧请求不能通过迟到成功错误地关闭已经打开的熔断器。
- Client 在每次 attempt 前执行熔断准入，按错误类型更新状态，并在 Open 时直接返回 `UNAVAILABLE`。
- `client.Stats` 增加 `RetryAttempts`、`RetriedCalls`、`CircuitState`、`CircuitRejected` 和 `CircuitOpens`。
- Echo 示例客户端将只读调用标记为幂等，用于展示 `rpc.WithIdempotent` 的使用方式。
- 新增 `docs/retry-and-circuit-breaker.md`，说明重试风暴、幂等性、退避公式、错误集合和状态机。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过 |
| `go test -race -count=3 ./internal/governance ./internal/client ./pkg/rpc` | 通过 |
| `golangci-lint run` | 通过 |
| Echo Server 与 Echo Client 真实调用 | 通过，客户端输出 `hello retry and breaker` |

关键测试结果：

- `TestRetryPolicyRetriesIdempotentTransientErrors`：前两次返回 `UNAVAILABLE`，第 3 次成功，输出 attempt 数为 3。
- `TestRetryPolicyRejectsNonIdempotentAndPermanentErrors`：非幂等调用和 `INVALID_ARGUMENT` 均只执行 1 次。
- `TestRetryPolicyHonorsCallDeadline`：调用 deadline 到期后停止退避，不会耗尽最大 attempt 数。
- `TestRetryPolicyHonorsMaxElapsedWithoutExtraWait`：下一次退避会超过总预算时立即返回。
- `TestRetryBackoffIsExponentialCappedAndJittered`：验证 10ms、20ms 指数增长、40ms 上限和可注入抖动。
- `TestCircuitBreakerStateTransitions`：验证 Closed → Open → HalfOpen → Closed，以及探测失败回到 Open。
- `TestCircuitBreakerAllowsOnlyOneConcurrentHalfOpenProbe`：64 个并发 HalfOpen 请求中只有 1 个探测被放行。
- `TestCircuitBreakerIgnoresLateClosedCallResultAfterOpening`：打开前的迟到成功不会错误恢复 Closed。
- `TestIdempotentCallRetriesUnavailable`：真实 TCP Echo 调用执行 3 次 attempt，统计为 3 次 attempt、1 次重试调用。
- `TestNonIdempotentCallDoesNotRetry`：未标记幂等的真实调用只执行 1 次。
- `TestCircuitBreakerDegradesAndRecovers`：注入两次不可用后打开熔断，第三次调用未到达 Handler；解除故障并冷却后探测成功恢复 Closed。
- 重试、熔断和客户端集成测试重复 3 次并通过 race detector。

## 验收

- [x] 可重试错误集合和非重试错误集合明确。
- [x] 最大 attempt 数、指数退避、随机抖动和总耗时预算已实现。
- [x] 只有显式幂等请求允许重试，非幂等请求不会自动重放。
- [x] 所有 attempt 共享原始 Context deadline。
- [x] Closed、Open、HalfOpen 状态机和冷却探测已实现。
- [x] Open 状态快速拒绝，HalfOpen 只允许一个探测请求。
- [x] 探测成功恢复 Closed，探测失败重新 Open。
- [x] 并发状态切换和 64 路 HalfOpen 准入通过竞态检测。
- [x] attempt 数量和重试调用数可以从 `client.Stats` 和测试输出观察。
- [x] 通过可控 `UNAVAILABLE` 故障注入演示降级和恢复。
- [x] 重试与熔断说明已写入 `docs/retry-and-circuit-breaker.md`。
- [x] 实现和说明使用独立语义提交。
- [ ] GitHub Actions 推送后的远端运行结果：推送后确认。

## 已知限制

- 当前 Client 绑定单一地址，重试仍访问同一地址，尚未与 Resolver/Balancer 联动切换实例。
- 熔断状态保存在单个 Client 进程内，不跨进程或跨实例共享。
- 框架只识别错误码，不能区分“确定未执行”和“响应丢失、执行状态未知”。
- 幂等性必须由业务显式声明，框架不会自动推断写操作的安全性。
- `MaxElapsed` 只约束重试等待和总预算，单次网络操作仍受调用 Context 和传输 deadline 约束。
- 熔断指标目前是快照计数，Prometheus 输出在阶段 10 接入。

## 下一阶段依赖

阶段 9 限流和资源保护应复用本阶段的治理位置：

- 限流应在熔断准入之后、连接和网络写入之前执行。
- 限流拒绝使用 `RESOURCE_EXHAUSTED`，但不应直接计为熔断失败。
- 等待令牌时必须遵守原始调用 deadline，并能被显式取消。
- 重试 attempt 不能绕过方法级或服务级限流。
- 新增限流统计不得破坏现有重试和熔断状态快照。
