# Protobuf 与代码生成

Mini-RPC v1 使用 Protobuf 定义消息和服务，并生成两类 Go 文件：

| 生成器 | 输出 | 职责 |
|---|---|---|
| protoc-gen-go | 消息文件 *.pb.go | 消息类型、序列化方法和文件描述符 |
| protoc-gen-minirpc | 适配器文件 *_minirpc.pb.go | 客户端接口、服务端接口、方法表和注册函数 |

## 工具版本

生成结果属于源码，必须可复现。当前固定版本如下：

- Go 基线：1.23
- protoc：28.3
- protoc-gen-go：v1.36.6
- protoc-gen-minirpc：仓库内 cmd/protoc-gen-minirpc

make tools 把两个 Go 插件安装到仓库的 bin/ 目录，该目录不会提交。protoc 需要由开发环境、系统包管理器或 CI action 安装。

## 命令

~~~bash
make tools
make proto
make proto-check
~~~

make proto 读取 api/ 下全部 .proto 文件，执行以下语义：

~~~text
api/echo/v1/echo.proto
  -> api/echo/v1/echo.pb.go
  -> api/echo/v1/echo_minirpc.pb.go
~~~

make proto-check 重新生成全部文件，并使用 git status --porcelain -- api 验证工作区中没有生成差异。生成文件必须先与 .proto 一起提交，否则检查会失败。

## Echo API

api/echo/v1/echo.proto 定义：

- 包名：rpc.echo.v1
- Go 包：echov1
- 服务：Echo
- 方法：Echo(EchoRequest) returns (EchoResponse)

当前方法为第 3 阶段真实 TCP 调用准备的协议无关接口，不包含传输实现。

## 生成符号

以 Echo 服务为例，生成器提供：

- EchoServiceName：线上使用的完全限定服务名。
- EchoServer：业务实现需要满足的类型安全服务端接口。
- EchoClient：生成给业务调用的客户端接口。
- NewEchoClient：包装稳定 rpc.ClientConn 接口的构造函数。
- EchoServiceDesc：服务端分发所需的方法表和适配器。
- RegisterEchoServer：把服务实现注册到 rpc.ServiceRegistrar。

生成客户端不会直接依赖 TCP、连接池或 etcd。它只依赖 pkg/rpc.ClientConn.Invoke，因此协议层和治理层可以在后续阶段独立演进。生成服务端方法表只依赖 pkg/rpc.ServiceDesc 与 rpc.ServiceRegistrar。

## 生成规则

- v1 只生成一元 RPC；检测到客户端流或服务端流时生成器直接失败。
- 业务代码不得手工复制或修改服务方法分发表。
- 生成文件头部包含 DO NOT EDIT，字段和方法以 .proto 为唯一来源。
- 新增 protobuf 文件必须声明 go_package，并允许 paths=source_relative 生成。
- 修改生成器模板后必须重新执行 make proto，审查生成差异并提交。
- 运行 make check 前必须安装对应版本的 protoc。

## 测试

生成器单元测试验证：

- 一元服务可以生成服务名、客户端/服务端接口、注册函数和方法表。
- 流式方法被明确拒绝。

api/echo/v1/echo_minirpc.pb.go 的接口测试验证：

- 生成客户端把正确的服务名、方法名、请求和响应对象传给 ClientConn。
- 注册函数把完整服务描述和业务实现交给 ServiceRegistrar。
- 生成方法表可以调用类型安全的服务端实现。
