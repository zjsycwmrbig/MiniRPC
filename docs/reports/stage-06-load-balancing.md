# 阶段 6 报告：负载均衡

## 阶段

阶段 6：在 Resolver 实例快照上实现 Round Robin 和 Weighted Round Robin。

## 主提交

- `4d68356 feat(balancer): add round robin and weighted selection`

本报告使用独立文档提交记录上述实现基线，便于后续批次追踪和问题定位。

## 实现

- 定义只消费实例切片、不访问 Registry 的 `Balancer` 接口。
- 实现线程安全的 `RoundRobin`，按快照输入顺序稳定轮转。
- 实现 smooth `WeightedRoundRobin`，使用实例 ID 保存公平状态。
- 实例新增、删除和权重修改后保留存活实例状态，并清理已下线 ID。
- 权重小于等于 0 的实例不参与加权选择。
- 空列表、全部零权重和重复 ID 返回稳定错误。
- 新增 `docs/load-balancing.md`，记录算法、错误和 benchmark 入口。

## 测试

使用 Go 1.23.12 和 golangci-lint v1.64.8 验收。

| 命令 | 结果 |
|---|---|
| `make check` | 通过 |
| `make race` | 通过 |
| `go test ./internal/balancer -count=3` | 通过 |
| `go test -race ./internal/balancer -count=3` | 通过 |
| `go test ./internal/balancer -run '^$' -bench . -benchmem -benchtime=100000x` | 通过 |
| `golangci-lint run` | 通过 |

关键结果：

- Round Robin 6 次选择稳定得到 `a,b,c,a,b,c`。
- 600 个并发 Pick 在三个实例上各得到 200 次。
- 60 万次 `1:2:3` 加权选择精确得到 10 万、20 万、30 万。
- 从快照移除 `a` 后不再选择 `a`，新权重 `b:c=3:1` 在 400 次选择中得到 300/100。
- 100000 次 benchmark：
  - Round Robin：约 271 ns/op，0 allocs/op。
  - Weighted Round Robin：约 637 ns/op，0 allocs/op。
  - 加权分布：`a=0.1667`、`b=0.3333`、`c=0.5000`。

## 验收

- [x] Balancer 接口完成。
- [x] Round Robin 完成并验证顺序。
- [x] Weighted Round Robin 完成并验证比例。
- [x] 实例添加、删除和权重变化行为明确。
- [x] 空实例列表返回 `ErrNoInstances`。
- [x] 并发 Pick 通过竞态检测。
- [x] 选择统计超过 10 万次。
- [x] benchmark 输出各实例选择比例。
- [x] 阶段实现和验收报告使用独立提交。
- [ ] GitHub Actions 推送后的远端运行结果：推送后在新一轮审阅中确认。

## 已知限制

- 纯选择和 RPC 调用链的自动接线属于后续治理阶段。
- 健康检查和主动摘除尚未实现。
- 当前以完整快照替换状态，不提供单实例可变更新接口。

## 下一阶段依赖

阶段 7 超时与取消需要复用当前稳定快照和连接池：

- Deadline 应同时约束连接获取、写入和响应等待。
- 取消后必须清理 pending 状态和连接池并发槽。
- 超时和取消测试不得依赖外部网络。
- 保持 Balancer 在空快照和并发 Pick 下的错误语义。
