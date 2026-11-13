# Mini-RPC 架构概览

本文档记录仓库初始化阶段已经确定的模块边界、调用链和工程约束。完整协议、治理策略与验收标准见 [技术设计](mini-rpc-technical-design.md)。

## 目标与非目标

v0.1.0 的目标是形成一套可在本地多进程环境运行和验证的 Go RPC 框架：

- 使用 Protobuf 定义服务，并通过生成代码提供客户端和服务端接口。
- 在 TCP 长连接上使用自定义二进制帧，支持一元 RPC 调用和请求并发复用。
- 使用 etcd 完成服务注册、健康续租和发现，并通过本地快照支撑高并发解析。
- 提供轮询、加权轮询、超时、取消、有限重试、熔断和本地令牌桶限流。
- 输出结构化日志、Prometheus 风格指标和 Trace，并支持优雅关闭。
- 通过竞态测试、故障注入和基准测试证明关键机制有效。

v0.1.0 不实现完整 HTTP/2、流式 RPC、多语言代码生成、生产级认证、自研 Raft 或分布式事务。

## 总体结构

~~~mermaid
flowchart TB
    App[业务应用]
    Client[RPC Client]
    Server[RPC Server]
    Resolver[Resolver 与 Balancer]
    Registry[Registry Adapter]
    Etcd[etcd]
    Governance[超时 / 重试 / 熔断 / 限流]
    Transport[TCP Transport 与帧协议]
    Observe[日志 / 指标 / Trace]

    App --> Client
    App --> Server
    Client --> Governance
    Governance --> Resolver
    Resolver --> Registry
    Registry --> Etcd
    Governance --> Transport
    Transport --> Server
    Server --> Registry
    Client --> Observe
    Server --> Observe
~~~

客户端在治理链完成实例选择、限流和熔断判断后，通过每实例有界连接池发出请求。连接池复用多路传输并回收空闲连接；服务端从同一连接读取多个请求，基于 RequestID 并发关联响应，同时通过 Registry Lease 维持实例可用性。

## 分层与职责

| 层 | 包 | 职责 | 稳定边界 |
|---|---|---|---|
| 用户 API | <code>pkg/rpc</code> | 暴露 Client、Server、Resolver、Balancer 等稳定接口 | 对业务代码稳定 |
| 生成代码 | <code>api/</code> | <code>.proto</code>、生成请求响应类型、服务注册和调用桩 | 由接口定义生成 |
| 可执行程序 | <code>cmd/</code> | Echo Server、Echo Client 和多实例示例 | 进程入口 |
| 调用编排 | <code>internal/client</code>, <code>internal/server</code> | 发起调用、服务分发和生命周期管理 | 不对外暴露 |
| 服务治理 | <code>internal/governance</code> | 超时、取消、重试、熔断和限流 | 可组合中间件 |
| 地址解析与选择 | <code>internal/resolver</code>, <code>internal/balancer</code> | 实例快照、Watch 更新和实例选择 | 快照读模型稳定 |
| 注册中心 | <code>internal/registry</code> | etcd Lease、KeepAlive、List 和 Watch | 通过接口隔离实现 |
| 网络与协议 | <code>internal/transport</code>, <code>internal/protocol</code> | TCP 长连接、分帧、RequestID 关联、单连接并发上限和协议校验 | 字节级协议稳定 |
| 编解码 | <code>internal/codec</code> | Protobuf 负载编解码 | 与协议帧解耦 |
| 可观测性 | <code>internal/observability</code> | 日志字段、指标和 Trace 生命周期 | 可关闭、无业务侵入 |
| 测试工具 | <code>internal/testkit</code> | 测试服务端、代理、延迟和故障注入 | 只服务测试与示例 |

<code>internal</code> 包允许实现演进，但同一批内已被多个组件依赖的接口必须先有测试再扩展。<code>pkg/rpc</code> 和协议字节布局属于高返工成本边界，修改时必须同步文档与兼容性测试。

## 关键调用链

### 客户端

~~~text
业务调用
  -> 生成 request_id / trace_id
  -> 解析服务实例快照并选择实例
  -> 检查熔断和限流
  -> 获取连接并分配 RequestID
  -> 编码帧并写入连接
  -> 等待响应、取消或 deadline
  -> 判定错误是否允许重试
  -> 更新指标和熔断状态
~~~

### 服务端

~~~text
接受 TCP 连接
  -> 读取固定头部并校验 Magic / Version / Type / Length
  -> 读取 Header 和 Protobuf Body
  -> 根据 service / method 查找生成的服务描述
  -> 创建带 deadline 和 Trace 的 Context
  -> 执行业务方法并捕获 panic
  -> 编码响应或错误
  -> 写回带同一 RequestID 的帧
  -> 记录指标与日志
~~~

## 协议与一致性约束

- 所有整数使用大端序；Magic 为 <code>0x4D52</code>，v1 版本号为 <code>1</code>。
- 同一 TCP 连接内 RequestID 唯一，用于将响应、取消和未完成请求关联起来。
- Header 上限为 64 KiB，Body 默认上限为 8 MiB；非法长度或版本必须终止连接。
- 连接关闭时必须唤醒全部 pending 请求，不能留下永久阻塞的调用。
- 超时后必须同时清理 pending 状态和等待资源；迟到的响应只能丢弃，不能重复投递。
- Resolver 读取本地实例快照，不能在每个 RPC 请求中同步访问 etcd。
- Watch 连接中断后必须重连并执行一次全量刷新，避免只依赖增量事件。
- 重试只适用于明确的临时错误和幂等请求，并同时受最大次数、最大总耗时与 Context deadline 限制。
- 指标标签不能包含 request_id 等无限增长的维度。

协议字节布局、Metadata 编码、错误分类和示例帧见 [二进制协议 v1](protocol.md)。

单连接并发调用、RequestID pending map、RPC ErrorCode 和 Echo 运行方式见 [TCP Transport 与单机调用](transport.md)。连接上限、空闲回收、断连恢复和服务端 GracefulStop 见 [连接池与优雅关闭](connection-pool.md)。Lease、Watch 与本地快照语义见 [服务注册与发现](registry.md)，实例选择策略见 [负载均衡](load-balancing.md)，分层超时见 [超时与请求取消](timeout.md)，有限幂等重试与三态熔断见 [重试与熔断](retry-and-circuit-breaker.md)。

## 测试边界

| 测试层 | 重点 |
|---|---|
| 单元测试 | 协议粘包拆包、状态机、连接池、负载均衡、令牌桶和快照更新 |
| 集成测试 | etcd 注册发现、多实例调用、实例下线、Watch 恢复 |
| 竞态测试 | 同连接并发调用、连接关闭、实例更新和熔断状态切换 |
| 故障注入 | 延迟、丢响应、断连、实例停止、注册中心抖动和资源耗尽 |
| 性能测试 | 串行、单连接并发、多连接并发和治理开关对比 |

测试不得依赖公网或固定机器地址。需要 etcd 或容器的集成测试应通过 Docker Compose 或 testcontainers 在本地受控环境中启动。

## 代码生成边界

- `.proto` 是消息、服务名、方法名和请求响应类型的唯一来源。
- `protoc-gen-go` 生成 Protobuf 消息与描述符；`protoc-gen-minirpc` 生成类型安全客户端、服务端接口、方法表和注册函数。
- 生成方法表包含请求消息构造函数，服务端据此从 Frame Body 解码类型安全的输入。
- 生成客户端只依赖 `pkg/rpc.ClientConn.Invoke`，不感知 TCP、连接池、注册中心或治理策略。
- 生成服务端方法表只依赖 `pkg/rpc.ServiceDesc` 和 `pkg/rpc.ServiceRegistrar`。
- v1 生成器拒绝流式方法；流式协议必须通过后续设计批次单独引入。
- 生成文件与 `.proto` 同批提交，由 `make proto-check` 防止手工修改或生成流程漂移。

详细命令和生成符号见 [代码生成](code-generation.md)。

## 工程约束

- 模块路径为 <code>github.com/zjsycwmrbig/mini-rpc</code>，Go 版本基线为 1.23。
- 本地统一入口为 Makefile；CI 执行格式、vet、测试、竞态测试、构建和 lint。
- 每个阶段只修改本阶段范围，并创建独立语义提交。
- 每个阶段在 <code>docs/reports/</code> 记录实现、测试、验收、已知限制和下一阶段依赖。
- 功能完成后才更新 README 中对应的运行说明，避免文档承诺尚未存在的命令。
