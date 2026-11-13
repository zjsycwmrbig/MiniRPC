# 限流与资源保护

阶段 9 在超时、重试、熔断和连接池之上增加本地令牌桶、服务端连接/并发/请求体保护，并为客户端调用链和服务端分发链提供一致的 `RESOURCE_EXHAUSTED` 拒绝语义。

## 设计目标

- 限制单个客户端或服务端进程的长期请求速率，同时允许配置范围内的短时突发。
- 同时支持服务级和方法级规则，避免重试 attempt 绕过限流。
- 支持立即拒绝和等待令牌两种模式；等待必须服从 Context deadline 和取消。
- 在 TCP 连接、Handler 并发和请求体三个层面保护服务端内存与 goroutine。
- 暴露允许、拒绝、等待、取消和剩余令牌快照，为阶段 10 的 Prometheus 指标提供稳定数据源。
- 限流拒绝不能被熔断器计为远端可用性失败，本地限流错误不能被客户端自动重试。

非目标包括分布式全局限流、跨进程共享令牌桶和基于 `Retry-After` 的服务端配额协商；这些能力留待后续阶段。

## 调用链位置

客户端每次 attempt 都经过相同治理顺序：

~~~text
Client.Invoke
  -> 补齐调用 deadline
  -> RetryPolicy.Do
       -> CircuitBreaker.Allow
       -> RateLimiter.Acquire(service, method)
       -> 获取连接池槽位
       -> 写入网络并等待响应
       -> 更新熔断状态
       -> 可重试时退避并开始下一次 attempt
~~~

限流位于熔断准入之后、连接池和网络写入之前。某个 attempt 被本地限流拒绝时不会访问网络；如果调用需要重试，下一次 attempt 必须重新获取熔断许可和令牌。

服务端在分发业务方法前执行资源准入：

~~~text
Accept TCP
  -> MaxConnections 准入
  -> 解码协议帧
  -> MaxConcurrentRequests 准入
  -> 查找 service / method
  -> 创建 deadline Context 并绑定 CANCEL
  -> MaxRequestBodyBytes 检查
  -> RateLimiter.Acquire(service, method)
  -> Protobuf 解码
  -> 执行业务 Handler
~~~

## 令牌桶算法

每个规则维护独立桶状态：

~~~text
tokens = min(Burst, tokens + elapsed_seconds * Rate)
~~~

请求需要消耗 1 个令牌。容量为 `Burst` 时，桶初始装满，因此最多允许 `Burst` 个瞬时请求；桶耗尽后，长期补充速度由每秒 `Rate` 个令牌约束。

整数令牌检查、按时间补充和扣减在同一把桶锁内完成。128 路并发测试确认最多只有 `Burst` 个请求成功，不会发生超发。

### 立即拒绝

`Wait=false` 时，没有令牌立即返回本地 `governance.ErrRateLimited`。客户端将该错误映射为 `RESOURCE_EXHAUSTED`，且不自动重试。

### 等待令牌

`Wait=true` 时，请求计算获得下一个令牌所需的等待时间并阻塞，不预占未来令牌。定时器到期后重新加锁补充和竞争令牌，因此多个等待者不会共同超发同一枚令牌。

- Context 取消时立即返回 `context.Canceled`，客户端最终得到 `CANCELED`。
- deadline 到期返回 `context.DeadlineExceeded`，客户端最终得到 `DEADLINE_EXCEEDED`。
- 等待者不保证 FIFO 公平性，只保证容量和速率上限。

## 服务级与方法级规则

方法键通过 `governance.MethodRateLimitKey(service, method)` 构造，格式为 `service/method`。例如：

~~~go
limits := governance.RateLimitConfig{
    Services: map[string]governance.RateLimitRule{
        echov1.EchoServiceName: {Rate: 100, Burst: 200},
    },
    Methods: map[string]governance.RateLimitRule{
        governance.MethodRateLimitKey(echov1.EchoServiceName, "Echo"): {
            Rate:  20,
            Burst: 40,
        },
    },
}
~~~

方法规则优先于服务规则。上例中 `Echo` 使用方法级桶，其他方法使用服务级桶；没有匹配规则的方法不受本地限流。

方法优先级避免一个高流量方法被迫共享整个服务桶，同时允许运维对单个方法收紧速率。若未来需要“服务总配额 + 方法子配额”双层同时生效，应增加原子多桶准入，而不是简单串行扣减两个桶。

## 客户端接入

`client.WithRateLimits` 在 `Client.Invoke` 内生效：

~~~go
clientConn, err := client.Dial(
    ctx,
    address,
    client.WithRateLimits(limits),
)
~~~

Echo Client 也可以通过参数演示服务级限流：

~~~bash
go run ./cmd/echo-client -addr 127.0.0.1:8080 -rate 10 -burst 20
~~~

本地限流失败不会记作熔断失败。熔断器 Permit 在令牌获取失败时被释放，而不是 `RecordFailure`。

## 服务端接入与资源上限

服务端通过 `server.NewWithOptions` 配置：

~~~go
rpcServer, err := server.NewWithOptions(
    server.WithMaxConnections(1024),
    server.WithMaxConcurrentRequests(512),
    server.WithMaxRequestBodyBytes(1<<20),
    server.WithRateLimits(limits),
)
~~~

| 保护项 | 默认值 | 行为 |
|---|---:|---|
| `MaxConnections` | 1024 | 超限 TCP 连接立即关闭，并增加 `ConnectionRejected` |
| `MaxConcurrentRequests` | 1024 | 超限请求返回 `RESOURCE_EXHAUSTED`，不创建 Handler |
| `MaxRequestBodyBytes` | 8 MiB | 已解码请求体超限时返回 `RESOURCE_EXHAUSTED`，不进入 Handler |
| 服务端限流 | 关闭 | 通过 `RateLimitConfig` 启用服务或方法规则 |

协议 Codec 仍在读取长度后、分配 Body 前执行 64 KiB Header 和 8 MiB Body 硬上限。`MaxRequestBodyBytes` 是业务分发层更细粒度的保护，建议配置为不大于协议 Body 上限。

Echo Server 的服务级限流示例：

~~~bash
go run ./cmd/echo-server \
  -addr 127.0.0.1:8080 \
  -rate 20 -burst 40 \
  -max-connections 256 \
  -max-concurrent-requests 128 \
  -max-request-body-bytes 1048576
~~~

## 错误语义

| 场景 | 返回码 | 是否自动重试 |
|---|---|---|
| 客户端本地立即拒绝 | `RESOURCE_EXHAUSTED` | 否 |
| 客户端等待期间取消 | `CANCELED` | 否 |
| 客户端等待期间 deadline 到期 | `DEADLINE_EXCEEDED` | 否 |
| 服务端并发、请求体或限流拒绝 | `RESOURCE_EXHAUSTED` | 仍遵循阶段 8 的远程错误重试集合 |

服务端 `RESOURCE_EXHAUSTED` 即使被幂等客户端重试，也不会绕过限流，因为每个新 attempt 都重新经过服务端 `RateLimiter.Acquire`。后续阶段可增加显式 `Retry-After` Metadata，以决定更精确的重试时间。

## 状态快照

每个已配置桶在 `RateLimits` 中公开稳定快照：

- `Key`、`Rate`、`Burst`、`Wait`：规则和桶标识。
- `Tokens`：查询时补充后的剩余令牌数。
- `Allowed`、`Rejected`：成功取令牌和立即拒绝次数。
- `Waited`、`WaitCanceled`：进入等待和等待期间 Context 结束的次数。

`client.Stats` 和 `server.Stats` 同时聚合 `RateLimitAllowed`、`RateLimitRejected`、`RateLimitWaits`。服务端还提供 `ConnectionRejected`、`ConcurrencyRejected` 和 `BodyRejected`。

阶段 10 已将限流和资源耗尽结果映射到以下 Prometheus 指标：

- `rpc_requests_total{outcome="rate_limited"}`：所有 `RESOURCE_EXHAUSTED` 调用。
- `rpc_requests_failed_total{error_code="resource_exhausted"}`：按稳定错误码查询。
- `rpc_rate_limited_total`：资源耗尽专用计数器。
- 指标只使用 `role`、`service`、`method` 等有界标签，不包含 `request_id` 或 `trace_id`，也不读取或扣除令牌。

## Benchmark

独立基准命令：

~~~bash
go test -run '^$' -bench BenchmarkRateLimiterAcquire -benchmem ./internal/governance
~~~

该基准使用共享方法桶和高并发 `RunParallel`，覆盖实际锁竞争路径。阶段 12 会补充完整 QPS、延迟分位和资源占用报告。

## 测试覆盖

- 桶容量允许配置的短时突发。
- 时间推进后按长期速率补充令牌。
- 方法规则优先于服务规则。
- 128 路并发获取不超发。
- 等待期间 Context 取消立即返回。
- 客户端拒绝发生在网络之前，Handler 调用数不增加。
- 重试 attempt 重新经过客户端限流。
- 服务端连接、并发和请求体上限分别生效。
- 服务端等待令牌时响应调用 deadline。
- 相关路径通过 race detector 和重复运行。

## 已知限制

- 限流状态只存在于单进程，不使用 Redis、etcd 或中心配额服务。
- 服务级和方法级规则采用覆盖关系，不是双层总配额。
- 等待者没有 FIFO 公平性保证。
- 客户端和服务端需分别配置，当前没有控制面下发能力。
- 限流和资源保护数据仍是进程内状态；阶段 10 已通过 `/metrics`、结构化日志和 Trace 关联导出请求结果，但进程重启后计数清零。
