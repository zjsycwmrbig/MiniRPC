# 重试与熔断

阶段 8 在超时、取消和连接池语义之上增加有限重试与三态熔断。目标是只对明确可恢复且显式声明幂等的调用重试，并在实例持续不可用时快速失败，避免重试风暴放大故障。

## 调用链位置

~~~text
生成客户端
  -> Client.Invoke
  -> 补齐总 deadline
  -> 读取 Context 幂等标记
  -> RetryPolicy.Do
       -> CircuitBreaker.Allow
       -> 获取本地服务级或方法级限流令牌
       -> 获取连接池槽位
       -> 写入 Request（retry_attempt Metadata）
       -> 等待响应
       -> 记录熔断成功 / 失败
       -> 可重试时指数退避
  -> 返回最终响应或错误
~~~

截止期限在进入重试前确定。所有 attempt 共享同一个 Context deadline，不会为每次重试重新获得完整超时。

## 幂等标记

只有显式标记为幂等的调用才允许重试：

~~~go
ctx = rpc.WithIdempotent(ctx)
response, err := echoClient.Echo(ctx, request)
~~~

`rpc.WithIdempotent` 使用 Context value 标记，不信任线上请求 Metadata。Echo 是只读回显方法，因此示例客户端将其标记为幂等；写操作只有在业务能证明重复执行安全时才应使用该标记。

没有幂等标记时，`RetryPolicy.Do` 只执行一次。尤其是请求已经写出但响应丢失时，服务端可能已经执行，不能由框架擅自重放。

## 可重试错误

| ErrorCode | 是否重试 | 原因 |
|---|---|---|
| `UNAVAILABLE` | 是 | 连接、实例或服务暂时不可用 |
| `RESOURCE_EXHAUSTED` | 是 | 临时资源容量不足 |
| `DEADLINE_EXCEEDED` | 是，但调用总 deadline 仍有效时才重试 | 可能是单次尝试失败 |
| `INVALID_ARGUMENT` | 否 | 请求本身非法 |
| `NOT_FOUND` | 否 | service 或 method 不存在 |
| `INTERNAL` | 否 | 业务错误可能已产生副作用，避免重复执行 |
| `CANCELED` | 否 | 调用方已经明确终止 |
| `PROTOCOL_ERROR` | 否 | 协议或连接状态不可信 |

当 Context 已经完成时，无论错误码是否在可重试集合中都会停止。原始 `*rpc.Error` 会保留给调用方，便于后续判断。

阶段 9 的客户端本地限流错误虽然对外映射为 `RESOURCE_EXHAUSTED`，内部保留 `governance.ErrRateLimited` 并明确禁止自动重试，避免本地拒绝立即放大为多个 attempt。服务端返回的普通 `RESOURCE_EXHAUSTED` 仍按本表处理，但每个 attempt 都会重新经过客户端限流和连接池准入。

## 重试策略

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `MaxAttempts` | 3 | 包含首次请求在内的最大 attempt 数 |
| `InitialBackoff` | 50ms | 第一次重试的基础等待 |
| `MaxBackoff` | 1s | 单次退避上限 |
| `MaxElapsed` | 2s | 从首次 attempt 开始计算的重试总预算 |
| Jitter | 基础退避的 0–50% | 默认随机抖动，降低并发重试同步 |

第 N 次失败的退避为：

~~~text
base = min(InitialBackoff * 2^(N-1), MaxBackoff)
delay = min(base + jitter, MaxBackoff)
~~~

如果下一次等待会超过 `MaxElapsed` 或 Context deadline，框架直接返回最后一次错误，不执行该等待。

每次请求写入 `retry_attempt` Metadata。首次为 `0`，第一次重试为 `1`，以此类推。服务端可以用于观测，但不能据此放宽鉴权、幂等或业务校验。

## 熔断状态机

~~~text
Closed --连续失败达到阈值--> Open
Open --冷却期结束--> HalfOpen（仅一个探测请求）
HalfOpen --探测成功--> Closed
HalfOpen --探测失败--> Open
~~~

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `FailureThreshold` | 5 | Closed 状态下连续可用性失败阈值 |
| `OpenTimeout` | 30s | Open 状态冷却时间 |

- Closed：请求正常放行，成功会清零连续失败数。
- Open：立即返回 `UNAVAILABLE`，不获取连接、不写网络。
- HalfOpen：只允许一个探测请求；其他并发请求快速拒绝。
- 探测成功进入 Closed；探测失败立即回到 Open 并刷新冷却时间。
- 已打开前发出的旧请求即使稍后成功，也不能把 Open 错误地恢复为 Closed。

`UNAVAILABLE`、`RESOURCE_EXHAUSTED` 和调用 deadline 仍有效的 `DEADLINE_EXCEEDED` 计入可用性失败。业务 `INVALID_ARGUMENT`、`NOT_FOUND` 等错误说明服务端仍在正常响应，不计入熔断失败。

## 可观测状态

`client.Stats` 当前暴露：

- `RetryAttempts`：所有调用实际执行的 attempt 总数。
- `RetriedCalls`：执行超过一次 attempt 的调用数。
- `CircuitState`：`closed`、`open` 或 `half_open`。
- `CircuitRejected`：未进入网络调用就被熔断器拒绝的次数。
- `CircuitOpens`：熔断器进入 Open 的累计次数。

阶段 10 会将这些状态转换为 Prometheus 指标、结构化日志和 Trace 属性。

## 故障注入示例

`TestCircuitBreakerDegradesAndRecovers` 使用可控业务 Handler 注入 `UNAVAILABLE`：

1. 连续两次失败打开熔断器。
2. 第三次调用在客户端快速拒绝，Handler 调用数不增加。
3. 注入故障解除并等待冷却。
4. HalfOpen 探测成功，状态恢复 Closed。

这说明客户端可以在服务持续不可用时降级为快速失败，并在服务恢复后自动探测恢复。

## 测试覆盖

- 幂等临时错误按配置重试，第三次 attempt 成功。
- 非幂等调用只执行一次。
- `INVALID_ARGUMENT` 等永久错误不重试。
- 调用 Context deadline 到期后停止退避和重试。
- 退避呈指数增长、受上限约束并附加抖动。
- Closed、Open、HalfOpen 和探测失败回 Open 状态迁移。
- 64 个并发 HalfOpen 请求中只有一个探测被放行。
- 旧请求迟到成功不会关闭已经打开的熔断器。
- 熔断打开时快速拒绝，冷却后探测成功恢复。
- 重试和熔断状态切换通过 race detector。

## 已知限制

- 当前 Client 绑定单一地址；重试会重新拨号同一地址，尚未与 Resolver/Balancer 联动切换实例。
- 熔断状态保存在单个 Client 进程内，不跨进程共享。
- 重试不会为每次 attempt 单独缩短 deadline；总预算由原始 Context 和 `MaxElapsed` 共同限制。
- 幂等性由业务显式声明，框架无法自动推断写操作是否安全。
- 当前仅区分错误码，不区分“确定未执行”和“响应丢失、执行状态未知”。
