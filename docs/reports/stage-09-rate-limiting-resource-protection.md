# 阶段 9 报告：限流和资源保护

## 阶段

阶段 9：实现本地令牌桶、服务级/方法级限流、立即拒绝与等待准入，并为服务端连接、并发 Handler 和请求体增加资源保护。

## 相关提交

- `ab0cf9c feat(governance): add token bucket rate limiting and resource guards`
- `cb92561 docs(governance): define rate limit and resource protection semantics`

本报告使用独立文档提交记录测试证据、验收结果和阶段 10 依赖，不改变已提交的实现与语义基线。

## 实现

- 新增并发安全的 `governance.RateLimiter`，每个服务级或方法级规则对应独立令牌桶。
- 令牌按 `elapsed_seconds * Rate` 补充，最多累积到 `Burst`；一次请求原子检查并扣减 1 个令牌。
- 支持 `Wait=false` 立即拒绝和 `Wait=true` 等待令牌两种模式。
- 等待者在 timer 到期后重新竞争令牌，不预占未来容量；128 路并发测试确认不会超发。
- 等待过程直接监听原始 Context；取消立即返回 `context.Canceled`，deadline 到期返回 `context.DeadlineExceeded`。
- 方法规则使用 `service/method` 键，并优先于服务级规则；未匹配方法保持不限流。
- 客户端在熔断准入之后、连接池获取和网络写入之前获取令牌，因此每个 retry attempt 都必须重新通过限流。
- 客户端本地拒绝保留 `governance.ErrRateLimited` 内部错误并禁止自动重试，对外统一映射为 `RESOURCE_EXHAUSTED`。
- 本地限流拒绝会释放熔断 Permit，不会把客户端自身的拒绝计为远端可用性失败。
- 服务端新增 `MaxConnections`、`MaxConcurrentRequests` 和 `MaxRequestBodyBytes` 三层保护。
- 服务端超限 TCP 连接立即关闭；超限并发请求和已解码请求体返回 `RESOURCE_EXHAUSTED`，不进入 Handler。
- 服务端在业务解码和 Handler 执行前获取服务级或方法级令牌。
- `client.Stats` 和 `server.Stats` 增加允许、拒绝、等待聚合计数，以及包含剩余 `Tokens` 的逐桶快照。
- `server.NewWithOptions` 对连接、并发、请求体和限流规则做 fail-closed 配置校验。
- Echo Server 和 Echo Client 增加 `-rate`、`-burst` 参数，Echo Server 同时支持三类资源上限参数。
- 新增 `BenchmarkRateLimiterAcquire`，使用 `RunParallel` 覆盖共享方法桶的锁竞争路径。
- 新增 `docs/rate-limiting-and-resource-protection.md`，说明调用链、令牌桶公式、错误语义、资源和指标边界。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过 |
| `go test -race -count=3 ./internal/governance ./internal/client ./internal/server` | 通过 |
| `golangci-lint run` | 通过 |
| `go test -run '^$' -bench BenchmarkRateLimiterAcquire -benchmem ./internal/governance` | 通过 |
| Echo Server/Client 真实进程调用 | 第一次成功，桶耗尽后第二次稳定返回 `RESOURCE_EXHAUSTED` |

本机 benchmark 结果：

```text
BenchmarkRateLimiterAcquire-144  770146  1823 ns/op  24 B/op  1 allocs/op
```

关键测试结果：

- `TestRateLimiterAllowsBurstThenRefillsAtConfiguredRate`：Burst 允许瞬时请求，时间推进后严格按 Rate 补充并回到拒绝状态。
- `TestRateLimiterMethodRuleOverridesServiceRule`：方法级规则覆盖服务级规则。
- `TestRateLimiterConcurrentAcquireDoesNotOversubscribe`：128 路并发只有配置的 10 个令牌被成功获取。
- `TestRateLimiterWaitHonorsContextCancellation`：等待中的 Acquire 在取消后立即返回，并记录等待和取消计数。
- `TestRateLimitConfigValidation`：拒绝缺少 service/method 分隔符和非法 bucket 配置。
- `TestLocalRateLimitIsNotRetryable`：本地限流 sentinel 不进入 RetryPolicy 重试集合。
- `TestClientRateLimitRejectsBeforeNetwork`：第三次调用在网络之前被拒绝，服务端 Handler 只执行两次。
- `TestRateLimitAppliesToEveryRetryAttempt`：第一次 attempt 到达 Handler 后，第二次 attempt 被本地桶拒绝，Handler 调用数保持 1。
- `TestServerRateLimitRejectsBeforeHandler`：服务端桶耗尽后返回稳定 `RESOURCE_EXHAUSTED`，Handler 调用数不增加。
- `TestServerConcurrencyLimitRejectsExcessHandlers`：达到 Handler 并发上限时直接拒绝，不执行第二个 Handler。
- `TestServerRequestBodyLimitReturnsResourceExhausted`：超过业务请求体上限时 Handler 调用数为 0。
- `TestServerRateLimitWaitHonorsCallDeadline`：服务端等待令牌期间调用 deadline 到期并立即返回。
- `TestConnectionLimitRejectsExcessTransports`：第二条超过上限的 TCP Transport 被关闭并计数。
- `TestNewWithOptionsValidatesResourceLimits`：非法资源上限返回 `ErrInvalidConfig`。
- 限流、客户端和服务端相关测试重复 3 次并通过 race detector。

## 验收

- [x] 本地令牌桶支持长期平均速率和短时 Burst。
- [x] 服务级和方法级限流规则均已实现。
- [x] 支持立即拒绝与等待获取令牌。
- [x] 限流错误对外使用稳定 `RESOURCE_EXHAUSTED`。
- [x] 并发获取令牌不发生超发。
- [x] 等待期间 Context 取消和 deadline 到期可以立即返回。
- [x] retry attempt 不能绕过客户端或服务端限流。
- [x] 本地客户端限流拒绝不会自动重试，也不会被熔断器计为失败。
- [x] 服务端连接数、并发 Handler 数和请求体大小均有可配置保护。
- [x] 暴露允许、拒绝、等待、取消和剩余令牌快照。
- [x] 提供独立的 `BenchmarkRateLimiterAcquire`。
- [x] 至少包含一种客户端限流和三种服务端资源保护策略。
- [x] 文档说明限流在客户端和服务端调用链中的准确位置。
- [x] 实现、说明和阶段报告使用独立语义提交。
- [ ] GitHub Actions 推送后的远端运行结果：推送后确认。

## 已知限制

- 令牌桶是单进程本地状态，不能限制多个 Client 实例或多个 Server 进程的聚合速率。
- 服务级和方法级规则为覆盖关系，不是服务总配额与方法子配额同时生效的双层模型。
- 等待者按锁竞争唤醒，不提供 FIFO 公平性。
- 服务端返回的普通 `RESOURCE_EXHAUSTED` 仍属于阶段 8 的可重试远程错误；每个重试都会重新经过两端限流，但尚未携带 `Retry-After`。
- `MaxRequestBodyBytes` 在业务分发层检查；协议 Codec 的 8 MiB Body 硬上限仍负责在分配前终止超限线上帧。
- 连接数超限通过关闭 TCP 表达，客户端可能观察到 `UNAVAILABLE`，而不是 RPC 级 `RESOURCE_EXHAUSTED`。
- 限流数据目前是进程内结构快照，Prometheus、结构化日志和 Trace 属性将在阶段 10 接入。

## 下一阶段依赖

阶段 10 可观测性应复用本阶段已聚合的稳定快照：

- 将允许、拒绝、等待、取消和剩余令牌映射为 Prometheus 指标。
- 将重试、熔断、连接池、连接拒绝、并发拒绝和请求体拒绝合并到统一指标注册与生命周期。
- 仅使用 service、method、状态等有界标签，禁止 request_id 等高基数标签。
- 结构化日志和 Span 需要记录最终错误码、attempt、限流规则键和等待耗时。
- 指标采集不能阻塞或重复扣减令牌；读取快照必须保持无副作用，仅进行时间补充。
- 需要增加指标开关和并发抓取测试，避免阶段 10 的导出器引入数据竞争。
