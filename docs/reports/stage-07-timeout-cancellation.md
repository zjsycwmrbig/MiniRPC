# 阶段 7 报告：超时和请求取消

## 阶段

阶段 7：实现分层超时、deadline 透传、CANCEL 帧和服务端 Context 取消。

## 相关提交

- `6b33bbf feat(rpc): propagate deadlines and send cancellation frames`
- `e88a977 fix(timeout): classify deadline failures by call phase`
- `ea5438f docs(timeout): define deadline and cancellation semantics`

本报告使用独立文档提交记录上述实现与修正基线，便于阶段 8 在稳定的超时和取消语义上继续开发。

## 实现

- 为没有显式 deadline 的调用增加默认 `CallTimeout`，默认值为 10 秒。
- 将调用 deadline 写入请求 Metadata，并由服务端转换为带 deadline 的 Context。
- 请求帧写入使用调用 deadline 或 10 秒兜底写入期限，并返回可分类的 `ErrWriteTimeout`。
- 连接建立超时包装为 `ErrDialTimeout`，与响应等待超时和连接池容量等待区分。
- 连接池等待被主动取消时返回 `CANCELED`；等待 deadline 到期时返回 `RESOURCE_EXHAUSTED`。
- 显式取消 Context 时，向原 RequestID 发送 CANCEL 帧。
- CANCEL 写入使用独立短超时，并在同连接后续 Request 之前刷新，避免控制帧越过请求。
- 服务端按连接维护 RequestID 到 Handler CancelFunc 的映射，支持 CANCEL 先于 Handler 绑定到达。
- 连接关闭、立即关闭和 GracefulStop 都会取消该连接上的全部在途 Handler Context。
- 服务端统计增加 `CancelFrames`，用于验证取消帧确实被接收。
- 新增 `docs/timeout.md`，明确各层职责、错误语义、资源保护和限制。
- 补充超时分层修正：连接建立、连接池获取、帧写入和响应等待不再全部表现为同一种错误。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过 |
| `go test -race -count=3 ./internal/client` | 通过 |
| `golangci-lint run` | 通过 |
| GitHub Actions `CI`（`b9df484`） | 通过 |

关键测试：

- `TestDeadlinePropagatesToServerContext`：客户端 deadline 到期时，服务端 Handler Context 同样以 `DeadlineExceeded` 结束。
- `TestCancelFrameCancelsServerContext`：显式取消触发 CANCEL 帧，服务端 Handler 观察到 `context.Canceled`。
- `TestFallbackCallTimeout`：无 deadline 的调用受默认 `CallTimeout` 约束。
- `TestWriteTimeoutIsReported`：请求写入超时返回 `ErrWriteTimeout`。
- `TestDialTimeoutIsClassified`：连接建立超时返回 `ErrDialTimeout`，并映射为 `DEADLINE_EXCEEDED`。
- `TestPoolWaitCancellationReturnsCanceled`：等待连接池容量时主动取消返回 `CANCELED`。
- `TestManyTimeoutsReleasePendingAndServerSlots`：128 个并发超时调用全部被服务端观察到，客户端 pending 归零、服务端 active request 归零，且连接可继续完成恢复调用。
- 超时和取消测试全部通过 race detector，重复运行三次无时序失败。

## 验收

- [x] 支持 Context deadline 并写入协议 Metadata。
- [x] 服务端创建带 deadline 的请求 Context。
- [x] 客户端超时后清理 pending map 和连接池并发槽。
- [x] 支持连接内 CANCEL 帧并保证后续请求顺序。
- [x] 服务端可以观察显式取消和 deadline 到期。
- [x] 连接建立、连接池等待、写入和响应等待超时语义已区分。
- [x] 大量超时请求后资源计数可以回到零。
- [x] 超时、取消和恢复路径通过竞态检测。
- [x] 超时分层和限制已写入 `docs/timeout.md`。
- [x] 阶段实现、修正和验收文档使用独立提交。
- [x] GitHub Actions 远端运行成功：[run 43386484575](https://github.com/zjsycwmrbig/MiniRPC/actions/runs/43386484575)。

## 已知限制

- 业务 Handler 必须主动监听 `ctx.Done()`，框架不会强制终止忽略 Context 的用户代码。
- CANCEL 是同一连接内尽力发送的控制帧，不具备跨代理持久化和重放能力。
- 连接池容量等待 deadline 当前映射为 `RESOURCE_EXHAUSTED`，通过消息区分等待超时。
- 尚未在 Trace 或 Metrics 中记录细粒度超时阶段。
- 重试策略尚未实现，因此超时后不会自动切换实例重试。

## 下一阶段依赖

阶段 8 重试与熔断应直接复用本阶段的稳定错误语义：

- 明确 `UNAVAILABLE`、`DEADLINE_EXCEEDED`、`RESOURCE_EXHAUSTED` 和 `CANCELED` 是否可重试。
- 重试必须继承原始总 deadline，不能为每次 attempt 重设完整超时。
- 每次 attempt 都应清理 pending、并发槽和取消状态。
- 熔断器需要区分连接失败、写入失败、响应等待超时和服务端业务错误。
