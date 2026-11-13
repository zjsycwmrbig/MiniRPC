# 阶段 2 报告：二进制帧协议

## 阶段

阶段 2：实现协议帧编码和解码

## 主提交

- cee9013 feat: implement binary rpc frame codec

## 实现

- 定义 22 字节固定头部：Magic、Version、Type、Flags、HeaderLength、BodyLength 和 RequestID。
- 全部多字节整数使用大端序。
- 定义 Request、Response、Cancel 和 Heartbeat 帧类型。
- 定义 Compressed、Error 和 OneWay 标志，拒绝保留位。
- 实现确定性 Metadata 编码：key 排序、uint32 长度前缀、空 key 与重复 key 校验。
- 增加 service、method、deadline、trace、span、content type、retry attempt 和 error code 保留 key。
- 实现 Header 64 KiB 硬上限和 Body 默认 8 MiB 可配置上限。
- 实现新缓冲分配的 Encode、处理短写的 Write，以及完整帧语义的 Read。
- 区分帧间干净 EOF 与帧内截断。
- 定义可通过 errors.Is/errors.As 分类的协议错误。
- 添加 docs/protocol.md，包含字段表、Metadata 格式、限制、错误矩阵和示例十六进制帧。
- 更新 README、架构概览和批次进度。

## 测试

| 命令 | 结果 |
|---|---|
| go test ./... | 通过 |
| go test -cover ./internal/protocol/... | 通过，语句覆盖率 89.7% |
| go test -race ./internal/protocol/... | 通过 |
| go test -race ./... | 通过 |
| go vet ./... | 通过 |
| go build ./... | 通过 |
| make proto-check | 通过 |
| make check | 通过 |
| golangci-lint run（v1.64.8） | 通过 |

测试覆盖：

- 编解码等价、固定头部字节序和 Metadata 确定性排序。
- 单字节 reader 模拟半头部拆包。
- 一次读取多帧并保持帧边界。
- 空 Metadata、空 Body 和干净 EOF。
- 固定头部、Metadata 与 Body 的截断读取。
- 非法 Magic、Version、Type 和 Flags。
- HeaderLength 与 BodyLength 超限。
- 自定义 Header/Body 限制及非法 codec 配置。
- Metadata key/value 长度前缀截断、空 key 和重复 key。
- Writer 短写与零长度写入。
- 64 个 goroutine 并发编码，结合竞态检测验证不共享可变缓冲。
- 四种 FrameType 均通过多帧测试。

所有测试只使用内存 bytes.Reader、bytes.Buffer 和自定义 reader/writer，不依赖真实网络。

## 验收

- [x] 固定头部、Metadata 和可变 Body 已完成。
- [x] 大端序、长度校验、四种帧类型和协议错误已完成。
- [x] 每种协议错误都有独立错误分类和触发测试。
- [x] 编码后解码得到等价结构。
- [x] 半个头部、多帧、截断帧和并发编码测试通过。
- [x] docs/protocol.md 包含字段表和示例十六进制帧。
- [x] go test -race ./internal/protocol/... 通过。

## 已知限制

- Compressed 标志和压缩算法扩展点已定义，但压缩/解压实现尚未加入。
- error_code 通过 Metadata 传递，RPC 状态机、错误对象和重试映射在后续批次实现。
- 单连接 1024 并发请求限制属于 Transport/Client 生命周期，不在本阶段实现。
- 当前协议层可以识别帧边界，但还没有 TCP 连接和真实 RPC 调用。

## 下一阶段依赖

阶段 3 必须使用本阶段 Codec 和 Frame：

- TCP Listener 与 Dialer。
- Server 连接读取循环和 Client 请求发送。
- RequestID 到 pending channel 的并发映射。
- 连接关闭时唤醒所有等待请求。
- Echo Server、Echo Client 和生成客户端的首次真实调用。
