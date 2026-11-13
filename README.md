# Mini-RPC

Mini-RPC 是一个面向 Go 微服务场景的可治理 RPC 框架项目。v0.1.0 将覆盖 Protobuf 接口与代码生成、TCP 长连接与一元 RPC、服务注册发现、负载均衡、超时取消、重试熔断、令牌桶限流、可观测性、优雅关闭，以及集成测试、故障注入和性能基准。

当前处于**阶段 2：二进制 RPC 帧协议**。仓库已具备 Go module、Echo 接口与代码生成、确定性 Metadata 编码、大端序分帧、帧长度保护和协议错误分类；TCP Transport 与真实调用会按 [执行计划](docs/execution-plan.md) 继续推进。

## 环境要求

- Go 1.23 或更高版本
- GNU Make
- Git
- Protocol Buffers compiler（`protoc` 28.3 或兼容版本）

后续阶段会按需增加 Docker Compose 和 etcd 等工具。Go 代码生成器由 `make tools` 安装到被 Git 忽略的 `bin/` 目录：

- `protoc-gen-go` v1.36.6
- 项目内置的 `protoc-gen-minirpc`

## 快速开始

克隆或进入仓库后，先检查 Go module，生成 API 代码，再执行本地 CI 等价检查：

```bash
go version
go env GOMOD
make tools
make proto
make check
```

当前阶段还没有 RPC 示例可运行；最小可验证行为是 API 可以稳定重新生成，协议可以完成内存中的分帧与拆包，生成代码测试、协议测试、格式检查、静态检查、单元测试和构建全部通过。阶段 3 完成 TCP 单机调用后会补充 Echo Server 和 Client 的运行方式。

如果修改了 Protobuf 定义，请先执行 `make proto`，提交更新后的生成文件，再运行 `make proto-check` 确认重新生成不会产生差异。

查看全部开发命令：

```bash
make help
```

常用命令：

| 命令 | 用途 |
|---|---|
| `make tools` | 安装固定版本的 Go 代码生成器 |
| `make proto` | 从 `.proto` 生成消息类型和 Mini-RPC 适配器 |
| `make proto-check` | 检查已提交的生成代码是否与 `.proto` 一致 |
| `make fmt` | 格式化全部 Go 文件 |
| `make fmt-check` | 检查格式，不修改文件 |
| `make vet` | 运行 `go vet` |
| `make test` | 运行单元测试 |
| `make race` | 开启竞态检测运行测试 |
| `make build` | 编译全部 Go 包 |
| `make check` | 运行生成一致性、格式、vet、测试和构建检查 |
| `make lint` | 运行 golangci-lint（需要本机安装） |

## 目录结构

```text
MiniRPC/
├── api/                  # Protobuf API 和生成代码
├── cmd/                  # Echo Server、Client 等可执行程序
│   └── protoc-gen-minirpc/ # Mini-RPC 客户端与服务端适配器生成器
├── internal/             # 协议、传输、治理、注册中心等内部实现
│   ├── balancer/         # 负载均衡策略
│   ├── client/           # 客户端调用与连接选择
│   ├── codec/            # 序列化编解码
│   ├── governance/       # 超时、重试、熔断和限流
│   ├── observability/    # 日志、指标和 Trace
│   ├── protocol/         # 二进制帧协议
│   ├── registry/         # etcd 等注册中心适配器
│   ├── resolver/         # 客户端实例快照
│   ├── server/           # 服务端分发与生命周期
│   ├── testkit/          # 测试服务器、代理和故障注入工具
│   └── transport/        # TCP 连接与请求关联
├── pkg/rpc/              # 稳定的对外 API
├── examples/             # 可运行示例
├── benchmark/            # 压测程序和报告
├── fault-injection/      # 故障注入场景
├── deploy/               # Docker Compose 与本地依赖
├── docs/                 # 设计、协议、运维和阶段报告
├── Makefile              # 统一开发入口
└── go.mod                # Go module
```

`pkg/rpc` 只暴露稳定用户接口，具体实现保留在 `internal/`。空目录中的占位文件只用于保留计划结构，功能代码会在对应阶段加入。

## 文档

- [架构概览](docs/architecture.md)
- [代码生成](docs/code-generation.md)
- [二进制协议](docs/protocol.md)
- [技术设计](docs/mini-rpc-technical-design.md)
- [批次执行计划](docs/execution-plan.md)

## 完成批次

按设计文档的验收边界，v0.1.0 需要 **13 个实现批次（阶段 0 至阶段 12）**。阶段 12 同时承担最终工程化和发布验收，因此不再另设一个实现批次。

## 开发原则

- 一次只推进一个批次，并保持前置批次验收通过。
- 新增对外接口时同时补充测试和设计说明。
- 并发组件必须通过 `go test -race`。
- 每个阶段保留独立提交、测试结果、已知限制和下一阶段依赖。
- 最终以格式、vet、测试、竞态测试、构建、Docker Compose 集成测试和 benchmark 报告共同作为完成依据。

## License

[MIT](LICENSE)
