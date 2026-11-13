# 阶段 0 报告：工程初始化

## 阶段

阶段 0：建立仓库和工程规范

## 主提交

- <code>bbf810b chore: initialize mini-rpc repository</code>

本报告使用独立文档提交记录上述主提交，便于后续批次定位实现基线。

## 实现

- 初始化 Go 1.23 module：<code>github.com/zjsycwmrbig/mini-rpc</code>。
- 创建 <code>api/</code>、<code>cmd/</code>、<code>internal/</code>、<code>pkg/rpc/</code>、<code>examples/</code>、<code>benchmark/</code>、<code>fault-injection/</code>、<code>deploy/</code> 和 <code>docs/reports/</code> 目录。
- 为内部模块建立仅含包职责说明的编译骨架，未提前引入业务实现。
- 添加 Makefile，统一格式、格式检查、vet、测试、竞态测试、构建、lint 和 clean 命令。
- 添加 <code>.golangci.yml</code>、<code>.gitignore</code> 和 MIT LICENSE。
- 添加 GitHub Actions，执行格式、vet、单元测试、竞态测试、构建和 golangci-lint。
- 完善 README，记录环境要求、当前阶段、快速验证、目录职责和 13 批次计划。
- 添加 <code>docs/architecture.md</code>，记录模块边界、调用链、协议约束、测试边界和工程规则。

## 测试

本地 Go 1.22.2 低于设计基线，因此使用临时 Go 1.23.12 工具链验收。

| 命令 | 结果 |
|---|---|
| <code>go mod tidy</code> | 通过，无外部依赖 |
| <code>make check</code> | 通过，包含格式检查、vet、测试和构建 |
| <code>make race</code> | 通过，当前包无测试文件，竞态检测正常完成 |
| <code>make help</code> | 通过，目标列表和说明可正常输出 |
| <code>golangci-lint run</code>（v1.64.8） | 通过 |
| YAML 解析检查 | 通过 |

<code>make check</code> 实际覆盖：

~~~text
gofmt -l
go vet ./...
go test ./...
go build ./...
~~~

## 验收

- [x] Go module 和目录结构与设计一致。
- [x] Makefile、lint 配置、忽略规则和许可证已添加。
- [x] README 可以指导开发者执行当前阶段的最小验证。
- [x] 架构文档记录总体结构、边界和关键不变量。
- [x] 本地 CI 等价检查、竞态测试、构建和 lint 通过。
- [x] 独立语义提交已创建。
- [ ] 远端 GitHub Actions 实际运行：仓库尚未配置远端，当前仅完成工作流配置与本地等价验证。

## 已知限制

- 当前没有可运行的 RPC 示例；真实 Echo Server 和 Client 在阶段 3 加入。
- <code>api/</code>、<code>cmd/</code>、<code>examples/</code>、<code>benchmark/</code>、<code>fault-injection/</code> 和 <code>deploy/</code> 当前只有目录占位，不包含功能代码。
- 宿主默认 Go 为 1.22.2；开发环境需安装 Go 1.23+，否则 <code>go.mod</code> 会拒绝构建。
- 尚未连接 Git 远端，因此无法在本地触发 GitHub Actions。

## 下一阶段依赖

阶段 1 必须基于本阶段 module 和目录结构完成：

- 添加 <code>api/echo/v1/echo.proto</code>。
- 固定 Protobuf 和 Go 代码生成版本。
- 提供可重复执行的 <code>make proto</code> 与 <code>make proto-check</code>。
- 保留生成代码检查，不允许手工修改生成产物。
