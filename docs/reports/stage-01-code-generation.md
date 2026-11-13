# 阶段 1 报告：Protobuf API 与代码生成

## 阶段

阶段 1：定义 Protobuf API 与生成流程

## 主提交

- 269d71c feat: add echo service definition and code generation

## 实现

- 添加 api/echo/v1/echo.proto，定义 rpc.echo.v1.Echo 一元服务和 EchoRequest、EchoResponse。
- 添加 cmd/protoc-gen-minirpc 自定义 protoc 插件，生成类型安全客户端、服务端接口、方法描述和注册函数。
- 生成 api/echo/v1/echo.pb.go 和 api/echo/v1/echo_minirpc.pb.go。
- 在 pkg/rpc 增加 ClientConn、ServiceRegistrar、ServiceDesc、MethodDesc 和 MethodHandler 等稳定接口。
- 生成客户端只依赖 ClientConn.Invoke；生成服务端适配器只依赖 ServiceDesc 与 ServiceRegistrar。
- 拒绝客户端流和服务端流方法，明确保证 v1 的一元 RPC 边界。
- Makefile 增加 tools、proto、proto-check 目标，生成器版本通过 go.mod 固定。
- GitHub Actions 增加 protoc 28.3、生成代码一致性检查、vet、测试、竞态测试和构建。
- 添加代码生成说明并更新 README、架构概览和执行计划。

## 测试

| 命令 | 结果 |
|---|---|
| go mod verify | 通过 |
| go test ./... | 通过，生成器与 Echo 生成代码测试成功 |
| go test -race ./... | 通过 |
| go vet ./... | 通过 |
| go build ./... | 通过 |
| make proto | 通过，生成两个 Go 文件 |
| make proto-check | 通过，无生成差异 |
| make check | 通过 |
| golangci-lint run（v1.64.8） | 通过 |
| YAML 解析检查 | 通过 |

生成器测试覆盖一元适配器输出和流式方法拒绝。Echo 生成代码测试覆盖：

- 客户端传递正确的服务名、方法名、请求和响应对象。
- RegisterEchoServer 提供正确服务描述和实现。
- 服务端方法表可以执行类型安全分发。
- EchoRequest 可以通过 Protobuf 序列化和反序列化。

另外删除两个已提交的生成文件，再执行 make proto。文件成功重建，SHA-256 一致，git 工作区无差异，满足生成流程可重复的验收条件。

## 验收

- [x] 删除生成文件后可以由固定命令重新生成。
- [x] 业务代码不直接手写服务方法分发表。
- [x] 生成的客户端和服务端接口可以被示例、测试和后续服务端实现引用。
- [x] proto-check 可以检测生成文件与源定义不一致。
- [x] 生成代码、单元测试、竞态测试、vet、构建和 lint 通过。
- [ ] 远端 GitHub Actions 实际运行：仓库尚未配置远端，当前完成工作流配置与本地等价验证。

## 已知限制

- Echo 目前只有接口和生成代码，尚不能完成真实网络调用；TCP 调用在阶段 3 实现。
- v1 生成器只支持一元 RPC，流式方法会被明确拒绝。
- 生成服务端方法表已完成类型适配，但实际请求分发器要等阶段 3 的服务端实现。
- 当前 protoc 通过开发机或 CI action 安装，未把二进制提交到仓库。

## 下一阶段依赖

阶段 2 必须基于本阶段生成的服务名、请求响应类型和对外接口实现：

- 固定头部、Metadata 和 Protobuf Body 的二进制帧编码。
- 请求、响应、取消和心跳类型。
- Magic、Version、Type、HeaderLength 和 BodyLength 校验。
- 粘包、拆包、截断帧、越界长度和协议错误测试。
