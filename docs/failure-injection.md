# 故障注入与恢复验证

阶段 11 提供一个可重复执行的故障演练命令，用真实 TCP RPC、可控内存注册中心、帧级故障代理和服务端资源限制验证故障切换、恢复速度与治理边界。

## 一键运行

```bash
make fault-test
```

命令默认依次运行全部九个场景。也可以选择单个或多个场景：

```bash
go run ./fault-injection -scenario instance-failover,dropped-response
go run ./fault-injection -scenario all -timeout 20s -json
```

参数：

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-scenario` | `all` | `all` 或逗号分隔的场景名 |
| `-timeout` | `15s` | 整个演练进程的总超时 |
| `-json` | `false` | 输出机器可读 JSON；默认输出表格 |

任一场景不符合预期时，命令以非零状态退出，适合接入 CI、定时演练或发布前检查。

## 场景与预期

| 场景 | 注入方式 | 预期结果 |
|---|---|---|
| `instance-failover` | 停止当前轮询选中的实例 | 幂等调用在第二个健康实例成功，attempt 有上限 |
| `all-instances-down` | 停止全部实例 | 返回可识别的 `UNAVAILABLE`，不无限重试 |
| `instance-recovery` | 注销实例、启动新进程实例并重新注册 | Resolver 收到新快照后调用成功 |
| `response-error` | 替换首个响应为 `UNAVAILABLE` | 幂等请求重试，第二次尝试成功 |
| `dropped-response` | 丢弃首个响应帧 | 当前调用以 `DEADLINE_EXCEEDED` 结束，下一次独立调用恢复；未经确认的请求不被盲目重放 |
| `delayed-response` | 延迟首个响应超过该调用 deadline | 首次调用超时，迟到响应不影响下一次调用 |
| `forced-disconnect` | 转发请求前强制关闭 TCP 连接 | 幂等请求重新建连，第二次尝试成功 |
| `registry-outage` | 临时关闭 List 和 Watch，再恢复 | 保留最后可用快照；恢复后重新 List/Watch 并收到完整新快照 |
| `resource-exhaustion` | 占满服务端 Handler 并发槽 | 拒绝请求得到 `RESOURCE_EXHAUSTED`，未进入 Handler，attempt 有上限；释放容量后恢复 |

## 恢复时间定义

本文统一使用 **TTR（time to recovery）**：从注入故障开始，到首次健康 RPC 成功所经过的时间。具体场景略有调整：

- 实例切换、响应错误、延迟、丢响应和断连：从构造故障或发出受故障影响的调用开始，到健康响应返回。
- 实例恢复：从新实例开始重新注册，到 RPC 成功；其中包含 Watch/快照传播和连接建立。
- 注册中心中断：从注入不可用开始，到收到 `Err == nil` 且实例集合完整的新快照。该场景不发起 RPC，因此 attempts 为 0。
- 全部实例下线：不存在恢复，`RECOVERY` 列表示从首次 attempt 到有限次失败后返回错误的耗时，即快速失败延迟。
- 资源耗尽：从被拒绝请求开始，到释放容量后的健康调用成功。

这些值是单机确定性演练的观测，不是生产 SLO。网络、etcd 部署、调度和机器负载都会改变实际 TTR。

## 错误率定义

```text
error_rate = failures / attempts
```

- `ATTEMPTS` 是场景内全部 RPC attempt，包括首次请求和重试。
- `FAILURES` 是已经观测到的失败 attempt 或注入失败。
- 注册中心场景没有 RPC attempt，表格显示 `0.00%`；它的恢复判据是快照完整性和 List/Watch 调用计数，不是 RPC 错误率。
- 资源耗尽场景的两次 attempt 都被拒绝，因此错误率为 100%；释放阻塞 Handler 后的独立恢复调用不计入被拒绝调用的错误率。

## 真实结果

以下为一次本地完整演练输出。恢复时间是运行时观测，每次执行会有毫秒级差异。

```text
SCENARIO             RESULT  ATTEMPTS  FAILURES  ERROR_RATE  RECOVERY  DETAIL
instance-failover    PASS    2         1         50.00%      4ms       attempts=2 selected=b response="failover"
all-instances-down   PASS    2         2         100.00%     2ms       attempts=2 error=rpc: unavailable: dial tcp 127.0.0.1:45063: connect: connection refused elapsed=2ms
instance-recovery    PASS    1         0         0.00%       2ms       instances=1 response="instance-recovered" error=<nil>
response-error       PASS    2         1         50.00%      3ms       attempts=2 replaced=1 response="after-response-error" error=<nil>
dropped-response     PASS    2         1         50.00%      201ms     dropped=1 attempts=2 lost=rpc: deadline_exceeded: request deadline exceeded recovery="after-dropped-response"
delayed-response     PASS    2         1         50.00%      152ms     first=rpc: deadline_exceeded: request deadline exceeded recovery="after-delay" attempts=2
forced-disconnect    PASS    2         1         50.00%      3ms       attempts=2 connections=2 response="after-disconnect" error=<nil>
registry-outage      PASS    0         0         0.00%       5ms       instances=2 list_calls=2 watch_calls=2 snapshot_update=2026-11-13T09:18:55.141315275Z
resource-exhaustion  PASS    2         2         100.00%     2ms       bounded_attempts=2 handler_calls=2 rejected=rpc: resource_exhausted: server concurrency limit exceeded response="after-exhaustion"
```

## 实现结构

- `fault-injection/` 编排故障、断言结果、统计 attempt、错误率和 TTR，并输出表格或 JSON。
- `internal/testkit.FaultProxy` 是真实 TCP 帧级代理，可延迟、丢弃、替换响应或关闭连接，不需要修改业务 Handler。
- `internal/testkit.MemoryRegistry` 提供可控注册、注销、Watch、List、控制面不可用和 Watch 断连。
- `internal/discovery.Client` 在每次 attempt 从 Resolver 快照选择实例，并按 URL 地址缓存独立连接池。
- 同一个失败实例不会被同一次调用连续选择；新快照移除实例后，对应连接池会被清理。

## 治理边界

- 跨实例自动切换只允许显式标记 `rpc.WithIdempotent(ctx)` 的调用。
- 没有幂等标记时，即使错误码可重试，也只执行一次。
- 场景使用 `MaxAttempts=2`、`MaxElapsed=1s`；attempt、总耗时和调用 Context deadline 都是硬边界。
- `UNAVAILABLE`、服务端 `RESOURCE_EXHAUSTED` 和仍有效的 `DEADLINE_EXCEEDED` 才可能重试；本地限流拒绝不会重试。
- 响应丢失时无法证明服务端是否已经执行，当前调用不会基于同一 Context 继续重放；业务应在需要时使用安全的请求去重键和幂等语义。
- 重试、熔断和限流不能绕过上述次数与时间上限，因此不会互相触发无限循环。

## 已知限制

- 默认演练使用 `MemoryRegistry`，不会模拟 Lease TTL 自动过期；真实 etcd 生命周期仍由集成测试覆盖。
- `FaultProxy` 在完整协议帧边界操作，不注入帧内字节损坏、乱序、重复帧或网络分区。
- 场景按固定顺序运行，尚未引入随机混沌、长时间高并发故障或故障组合。
- 当前结果由客户端和测试组件直接统计；尚未把一次演练的历史 TTR 写入 Prometheus 或持久化报告。
- 丢响应场景只验证“当前调用安全失败、下一次调用恢复”，不自动推断写操作是否可重试。
