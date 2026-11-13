# 负载均衡

阶段 6 在 Resolver 的不可变实例快照上实现 Round Robin 与 Smooth Weighted Round Robin。

## 接口

```go
type Balancer interface {
    Pick(instances []registry.Instance) (registry.Instance, error)
}
```

Balancer 不持有 Resolver 指针，也不在 Pick 内访问 etcd。调用方传入当前快照，Resolver 更新与
调用选择只共享只读切片。

稳定错误：

- `ErrNoInstances`：实例列表为空。
- `ErrNoWeightedInstances`：加权策略收到的实例权重全部小于等于 0。
- `ErrInvalidInstances`：实例 ID 或地址为空，或同一次输入中存在重复 ID。

## Round Robin

`RoundRobin` 按输入顺序轮转。内部只保存无符号游标并使用互斥锁保护；输入长度变化时游标对
当前长度取模，因此实例增加或删除后仍可继续工作。

## Weighted Round Robin

`WeightedRoundRobin` 使用 smooth weighted round robin：

1. 每个实例的当前权重增加配置权重。
2. 选择当前权重最大的实例。
3. 被选实例减去所有正权重之和。

权重状态以实例 ID 为 key。实例新增、删除或权重修改后，存活实例的公平状态仍可继续使用；
已不存在实例的状态会被清理。权重小于等于 0 的实例不参与选择。

示例权重 `1:2:3` 的长期比例严格趋近 `1:2:3`，并避免同一大权重实例连续占满多个相邻槽位。

## 验收映射

| 验收项 | 证据 |
|---|---|
| Round Robin 顺序稳定 | `TestRoundRobinOrder` |
| 并发 Pick 无数据竞争 | `TestRoundRobinConcurrentPick` 与 `go test -race` |
| 权重比例正确 | 60 万次选择得到 10 万、20 万、30 万 |
| 实例下线后不再选择 | `TestWeightedRoundRobinTracksSnapshotChanges` |
| 空列表返回明确错误 | `TestBalancerRejectsEmptyAndInvalidInputs` |
| 至少 10 万次统计测试 | 60 万次加权选择测试 |
| benchmark 记录分布 | `BenchmarkWeightedDistribution` 输出各实例 `selected_ratio` |

运行 benchmark：

```bash
go test ./internal/balancer -run '^$' -bench . -benchmem
```

## 调用链接线

阶段 11 的 `internal/discovery.Client` 已把 Balancer 接入完整调用链。每次 attempt 都从 Resolver 的最新快照调用 `Pick`，并为每个地址维护独立连接池；默认为 Round Robin。跨实例重试只发生在显式幂等调用中，且同一次请求不会连续选择刚才失败的地址。实例从成功快照移除后，其连接池会被关闭和清理。

完整故障场景、错误率和恢复时间定义见[故障注入与恢复验证](failure-injection.md)。

## 当前限制

- 当前按快照和调用结果执行被动故障切换，没有主动健康检查或后台探测。
- 加权轮询可以通过 `discovery.Config.Balancer` 注入，Echo Client 命令行默认仍使用 Round Robin。
- 权重修改使用完整新快照，不提供单实例可变更新接口。
