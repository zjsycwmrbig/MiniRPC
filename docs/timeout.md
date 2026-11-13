# 超时与请求取消

阶段 7 为客户端调用链增加分层超时和显式取消。目标是让连接获取、帧写入、响应等待和服务端业务执行都有明确的时间边界，并在调用结束后及时释放 pending map、连接池并发槽和服务端请求状态。

## 超时层次

| 层次 | 默认值 | 责任方 | 触发后的行为 |
|---|---:|---|---|
| DialTimeout | 10s | Client 连接池 | 限制建立新 TCP 连接的时间，失败后可重新拨号 |
| CallTimeout | 10s | Client | 调用方未提供 deadline 时，为整个一元调用增加兜底 deadline |
| Write timeout | 调用 deadline；无 deadline 时 10s | Transport | 限制完整请求帧写入 TCP 的时间，返回 `ErrWriteTimeout` |
| Response wait | 调用 deadline | Transport | 等待匹配 RequestID 的响应；到期后删除 pending 项 |
| Deadline metadata | 调用 deadline | Client → Server | 将 Unix 纳秒 deadline 写入 `deadline_unix_nano` |
| Handler deadline | 请求 deadline | Server | 使用带 deadline 的 Context 执行生成方法表 Handler |
| Cancel write | 250ms | Transport | 尽力发送 CANCEL 帧，避免取消长时间占用连接写锁 |
| GracefulStop | 调用方指定 | Server | 停止接收新请求，等待在途请求；到期强制关闭连接 |

调用方显式提供 deadline 时，该 deadline 是总调用上限。各层不得把总 deadline 延长为多个串行超时之和。

## 客户端调用顺序

~~~text
检查或补充 Context deadline
  -> 写入 deadline metadata
  -> 在连接池中获取连接和并发槽
  -> 在调用 deadline 内写入完整请求帧
  -> 注册 RequestID pending channel
  -> 等待响应 / Context 完成 / 连接关闭
  -> 清理 pending 和连接池并发槽
~~~

等待连接池容量时，如果 Context 到期，返回 `RESOURCE_EXHAUSTED`，错误消息为 `connection pool capacity wait timed out`，表示调用被资源边界拒绝，而不是服务端业务超时；如果调用方主动取消，则返回 `CANCELED`。连接建立阶段的 Context deadline 由 `DialContext` 约束并包装为 `ErrDialTimeout`；请求已经写入后的 Context 到期则返回 `DEADLINE_EXCEEDED`。

## 响应等待与 pending 清理

一次请求在写入前注册 `map[RequestID]chan callResult`。以下任一事件都会结束该请求：

- 收到相同 RequestID 的成功或错误响应。
- 调用 Context 被取消或 deadline 到期。
- 连接写入失败。
- 连接关闭。

响应等待结束后会删除 pending 项并释放连接并发槽。同一 RequestID 的迟到响应找不到 pending 项时会被忽略，不会重复交付，也不会污染后续请求。

## CANCEL 帧

调用方显式取消 Context 时，Transport 会排队一个带原 RequestID 的 CANCEL 帧。CANCEL 帧具有以下约束：

- 取消写入使用独立 goroutine 和 250ms 超时，不能阻塞调用方返回。
- 同一连接上的下一请求写入前会先刷新已排队 CANCEL，避免 CANCEL 越过后续 Request。
- CANCEL 是尽力通知，不提供重放或持久化保证。
- deadline 到期走服务端 deadline Context，不额外发送 CANCEL 帧。

服务端维护每连接的 `RequestID -> CancelFunc` 状态。CANCEL 先到达时记录取消标记；Handler 尚未绑定 CancelFunc 时，绑定阶段会立即取消 Context。连接关闭和 GracefulStop 会取消该连接上的全部未完成 Handler。

## 错误语义

| 场景 | 客户端结果 |
|---|---|
| 调用 deadline 到期 | `DEADLINE_EXCEEDED` |
| 请求帧写入超时 | `DEADLINE_EXCEEDED`，消息标明写入超时 |
| 显式取消 Context | `CANCELED` |
| 连接池容量等待到期 | `RESOURCE_EXHAUSTED` |
| 连接建立超时 | `DEADLINE_EXCEEDED`，底层错误为 `ErrDialTimeout` |
| 服务端 Handler 返回 Context deadline | 错误响应 `DEADLINE_EXCEEDED` |
| 服务端 Handler 观察到 CANCEL | Context 以 `context.Canceled` 结束 |

## 资源保护

- 单连接最多同时保留 1024 个未完成请求。
- 超时或取消必须同时释放 transport pending、连接池 active 计数和服务端请求状态。
- 大量超时请求完成后，客户端 pending 必须回到 0，服务端 active request 必须回到 0。
- CANCEL 写入有独立短超时，失效连接会被关闭并由后续调用重新拨号。
- GracefulStop 和普通连接关闭都会取消在途 Handler，避免 Handler 永久等待。

## 已知限制

- 业务 Handler 必须主动监听 `ctx.Done()`；框架不会强制终止忽略 Context 的用户代码。
- 当前取消是连接内控制帧，不是可跨代理重放的事务性取消。
- 连接池容量等待超时统一映射为 `RESOURCE_EXHAUSTED`，等待原因通过错误消息区分。
- 尚未提供分布式 Trace 中的超时原因标签。

## 验收测试

- `TestDeadlinePropagatesToServerContext`：客户端 deadline 传递到服务端 Context。
- `TestCancelFrameCancelsServerContext`：显式取消通过 CANCEL 帧到达服务端。
- `TestFallbackCallTimeout`：无 deadline 调用使用默认 CallTimeout。
- `TestManyTimeoutsReleasePendingAndServerSlots`：128 个并发超时后 pending 和服务端 active request 归零，并恢复下一次调用。
- `TestWriteTimeoutIsReported`：请求写入超时映射为稳定的 transport 错误。
- 全部相关测试通过 race detector。
