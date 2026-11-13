# TCP Transport 与单机调用

本阶段实现单连接、多路复用的一元 RPC 调用。一个 TCP 连接可以同时承载多个请求，响应通过 RequestID 关联到对应调用。客户端在单连接之上使用有界连接池，详见 [连接池与优雅关闭](connection-pool.md)。

## 当前范围

- TCP Dialer 与 Listener。
- 单连接并发请求。
- RequestID pending map。
- 客户端取消后清理等待状态。
- 连接关闭时唤醒全部未完成请求。
- 单连接最多 1024 个并发请求。
- 生成客户端到 Echo Server 的真实 Protobuf RPC。
- RPC 业务错误码透传。
- 服务端 panic 转换为 INTERNAL。

服务实例切换、注册发现和治理调用链属于后续批次。

## 代码结构

| 包 | 职责 |
|---|---|
| internal/transport | 单流读写串行化、RequestID 分配、pending map、连接关闭唤醒 |
| internal/client | 构造请求 Metadata、Protobuf 编解码、等待响应、远端错误解析 |
| internal/server | 服务注册、连接读取循环、方法分发、并发执行、错误响应 |
| pkg/rpc | ClientConn、ServiceDesc、MethodDesc、RPC ErrorCode 与 Error |

生成客户端只依赖 pkg/rpc.ClientConn，因此后续 resolver、balancer 和连接池可以在不修改生成代码的情况下接入。

## 客户端调用链

~~~text
生成的 EchoClient
  -> Client.Invoke
  -> proto.Marshal(request)
  -> Metadata: service/method/content_type/deadline
  -> Transport.Conn.Call
  -> 分配非零 RequestID
  -> pending map 注册响应 channel
  -> 加写锁写出一个完整帧
  -> 等待响应 / Context 取消 / 连接关闭
  -> 解码成功 Protobuf 或 RPC Error
~~~

同一个 Client 可以被多个 goroutine 并发使用。

## 服务端调用链

~~~text
TCP Accept
  -> 每连接一个读取循环
  -> Codec 读取完整请求帧
  -> 每个请求启动分发 goroutine
  -> 查找 service/method
  -> NewRequest + proto.Unmarshal
  -> 创建带 deadline 的 Context
  -> 调用生成方法表 Handler
  -> marshal 响应或构造错误响应
  -> 加连接写锁写回同一 RequestID
~~~

读取顺序由单连接读取循环保证，业务 Handler 可以并发执行。所有响应使用 per-connection 写锁串行写入，避免帧字节交错。

## 线上 Metadata

请求帧：

| key | 值 |
|---|---|
| service | 完全限定服务名，例如 rpc.echo.v1.Echo |
| method | 生成方法名 |
| content_type | application/x-protobuf |
| deadline_unix_nano | Context 有 deadline 时的 Unix 纳秒 |

成功响应：

- Type=Response
- Flags=0
- Body 为 Protobuf 响应

错误响应：

- Type=Response
- Flags=Error
- error_code Metadata 为十进制 ErrorCode
- Body 为 UTF-8 错误消息

错误码：

| 值 | 名称 | 当前来源 |
|---:|---|---|
| 0 | OK | 保留 |
| 1 | INVALID_ARGUMENT | 非法参数、请求解码失败、未知 content type |
| 2 | NOT_FOUND | service 或 method 不存在 |
| 3 | DEADLINE_EXCEEDED | Handler 返回 Context deadline |
| 4 | UNAVAILABLE | 后续重试与可用性阶段使用 |
| 5 | RESOURCE_EXHAUSTED | 后续限流阶段使用 |
| 6 | INTERNAL | Handler 普通错误或 panic |
| 7 | CANCELED | Handler 返回 Context canceled |
| 8 | PROTOCOL_ERROR | 不支持的请求标志或协议响应异常 |

客户端通过 errors.As 获取 *rpc.Error，并检查 Code 与 Message。

## pending map 与并发

Transport.Conn 维护：

- 单调递增的非零 uint64 RequestID。
- map[RequestID]chan callResult。
- 每连接 write mutex。
- 单个响应读取 goroutine。

一次调用在发送前注册 channel，在以下任一事件后只清理一次：

- 收到匹配响应。
- Context 取消或超时。
- 写入失败。
- 连接关闭。

迟到响应找不到 pending 项时会被忽略。连接关闭会在 pending 锁内设置终态、清空 map，并向所有等待者发送连接错误，因此不会永久阻塞。

## Context 与取消

- 没有 deadline 时使用 Background Context。
- 有 deadline 时写入 deadline_unix_nano，服务端使用 WithDeadline。
- 客户端 Context 取消只清理本地 pending 状态并立即返回。
- CANCEL 帧和服务端主动上下文取消在阶段 7 中加入。

## 运行示例

终端一：

~~~bash
go run ./cmd/echo-server -addr 127.0.0.1:8080
~~~

终端二：

~~~bash
go run ./cmd/echo-client -addr 127.0.0.1:8080 -message "hello mini-rpc"
~~~

预期客户端输出：

~~~text
hello mini-rpc
~~~

## 测试覆盖

- net.Pipe 下的响应关联、并发 pending、取消清理和关闭唤醒。
- 真实 loopback TCP 下的生成客户端 Echo 调用。
- 单连接 100 个并发请求。
- 取消后的迟到响应不会污染下一次调用。
- 单连接并发槽达到上限时后续调用可被 Context 中止。
- 远端 INVALID_ARGUMENT 错误码和消息。
- service/method 不存在返回 NOT_FOUND。
- Handler panic 返回 INTERNAL。
- Server Close 唤醒客户端 pending 调用。
- 全部测试通过 race detector。

## 已知限制

- Cancel 只影响客户端等待，尚未向服务端发送取消帧。
- 尚未注册 etcd，也还没有负载均衡。
