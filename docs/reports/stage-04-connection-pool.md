# 阶段 4 报告：连接池、连接复用与优雅关闭

## 阶段

阶段 4：实现每实例有界连接池、连接复用、空闲回收、断连恢复和服务端 GracefulStop。

## 主提交

- `3c09251 feat(client): add bounded connection pool and graceful shutdown`

本报告使用独立文档提交记录上述实现基线，便于后续批次追踪和问题定位。

## 实现

- 将客户端从单连接封装升级为每地址有界连接池，默认最多 4 个活动连接和 2 个空闲连接。
- 每条 TCP 连接最多承载 1024 个并发请求，超出上限的调用等待可用槽位并受 Context 约束。
- 连接池优先复用具有剩余容量的连接；全部满载且未达到连接上限时按需建连。
- 达到连接上限后等待已有调用释放容量，Context 结束时稳定返回 `RESOURCE_EXHAUSTED`。
- 请求释放时压缩超额空闲连接；配置 `IdleTimeout` 后由后台回收器关闭长期空闲连接。
- 传输连接进入终态后，客户端移除失效连接、保留其他在途请求的明确错误，并按需重新拨号。
- 不在重拨后重放已经写入网络的请求，避免未知执行状态导致重复调用。
- 增加客户端连接数、活动请求数、拨号次数、回收次数、等待次数和关闭原因快照指标。
- 实现 `Server.GracefulStop(ctx)`：关闭 listener、停止接受新连接、等待已接受请求完成。
- draining 期间从既有连接读到的新请求返回 `UNAVAILABLE`。
- GracefulStop 在 Context 到期时标记 `graceful_timeout`、关闭连接并返回 `DeadlineExceeded`。
- 增加服务端接受连接数、活动请求数、完成请求数、拒绝请求数和关闭原因快照指标。
- Echo Server 收到 SIGINT 或 SIGTERM 后执行带可配置超时的 GracefulStop。
- 新增 `docs/connection-pool.md`，记录池策略、状态快照、断连语义和停止流程。

## 测试

使用 Go 1.23.12、protoc 28.3 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过，包含生成一致性、格式、vet、测试和构建 |
| `make race` | 通过，全部包在竞态检测下运行成功 |
| `go test ./...` | 通过 |
| `go test -race ./...` | 通过 |
| `golangci-lint run` | 通过 |
| Echo Server/Client 进程级调用与 SIGTERM 停止 | 通过，客户端输出 `stage4-smoke`，服务端退出码 0 |
| GitHub Actions `Verify` 与 `Lint` | 通过 |

测试覆盖的关键行为：

- 1000 个并发请求只建立 1 个 TCP 连接，客户端和服务端连接计数均验证为 1。
- 单连接并发上限为 1 时，超额调用在 Context 到期后返回 `RESOURCE_EXHAUSTED`。
- 两个连接完成任务后按 `max idle connections` 回收多余连接，关闭原因记录为 `idle_limit`。
- 首个服务端关闭连接后，同一客户端在服务恢复时重新拨号并完成新请求。
- GracefulStop 保留已经开始且未超时的请求，完成写回后以 `graceful` 原因退出。
- GracefulStop 超时时以 `graceful_timeout` 原因关闭连接并返回 `DeadlineExceeded`。
- transport 单连接并发槽满载时，等待方可以被 Context 中止。

## 验收

- [x] 每个服务地址具备有界连接池。
- [x] 1000 个请求不会创建 1000 个 TCP 连接。
- [x] 连接池达到上限时返回稳定、可测试的容量错误。
- [x] 空闲连接按数量上限和空闲时间回收。
- [x] 连接异常后后续请求可以重新建立连接。
- [x] GracefulStop 不丢失已接受且未超时的请求。
- [x] GracefulStop 有明确超时，不会永久阻塞调用方。
- [x] 连接数、活动请求数和关闭原因具备测试指标。
- [x] 阶段实现和验收报告使用独立提交。
- [x] GitHub Actions `Verify` 与 `Lint` 在提交 `69484d8` 上通过。

## 已知限制

- 连接池绑定单个服务地址，尚未接入实例快照和服务发现。
- 失效连接只在后续请求或释放路径被移除，没有独立心跳探活。
- 响应丢失等未知执行状态不自动重试，重试和幂等判断属于后续治理阶段。
- 当前指标是进程内快照，尚未接入 Prometheus 导出。
- GracefulStop 不会强行业务 Handler 遵守 Context；超时后关闭网络连接并返回，由业务自行保证及时退出。

## 下一阶段依赖

阶段 5 需要复用本阶段冻结的每地址连接池接口：

- 增加 Registry、Lease、KeepAlive、Watch 与 Resolver 快照。
- 将实例选择结果连接到每地址连接池，避免按请求同步访问 etcd。
- 在实例下线或 Watch 快照变化时关闭不再使用的池。
- 保持连接上限、并发上限、pending 清理和 GracefulStop 语义不变。
