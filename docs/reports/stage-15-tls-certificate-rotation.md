# 阶段 15 报告：TLS 证书热加载与安全轮换

## 结论

阶段 15 在阶段 14 的 TLS/mTLS 性能闭环上增加文件驱动的证书热加载。服务端证书、私钥和客户端 CA，以及客户端根 CA、客户端证书和私钥都可以在线更新。候选材料先完整解析和验证，再以原子指针发布；读取失败、PEM 损坏、证书/私钥不匹配或非法 CA 只增加失败状态，不覆盖 last-known-good。

本阶段保持 v1 帧协议、RPC 公开调用语义、连接池结构和 v0.1.0 稳定标签不变。新服务端材料用于后续 ClientHello，新客户端材料用于后续 Dial；已经建立的连接继续使用原握手身份直到自然关闭。

## 提交

- `0b84a08 feat(security): add validated atomic TLS certificate reload`
- `ca2ccfa feat(server): apply reloaded TLS material to new handshakes`
- `3520bf2 feat(client): use reloadable TLS material for new dials`
- `5619309 feat(observability): expose bounded TLS reload metrics`
- `9d2439f feat(cli): enable live TLS material polling`
- `677418c build(docker): add live TLS certificate rotation smoke test`
- `28227f1 ci(security): gate live TLS certificate rotation`

## 实现范围

- `internal/security.Reloader` 提供初始严格加载、文件内容签名去重、`Poll`、强制 `Reload`、`Run` 和防御性 `Current` 快照。
- `ReloadStats` 记录 active version、attempts、successes、failures、最近尝试/成功时间和最近错误；内容不变时 Poll 不创建新版本。
- reload 串行执行，候选配置通过全部校验后原子替换；回调在 reload 锁释放后调用。
- 服务端 `WithTLSConfigSource` 通过每个新 ClientHello 获取当前快照；固定配置的 `WithTLSConfig` 保持兼容。
- 客户端 `WithTLSConfigSource` 在每次新 Dial 获取当前快照、克隆配置并按目标地址补充 `ServerName`；固定配置的 `WithTLSConfig` 保持兼容。
- Echo Server/Client 增加 `-tls-reload-interval`，默认 `10s`，`0` 表示只在启动时严格加载。轮询随信号 Context 或客户端请求 Context 取消。
- 新指标为 `rpc_tls_reload_total{role,outcome}`、`rpc_tls_reload_failures_total{role}` 和 `rpc_tls_config_version{role}`，不包含路径、证书身份、错误文本或任意动态值。
- Docker 轮换 drill 使用一次性 CA、服务器 SAN、服务端证书和客户端证书，不提交私钥，不启用 `InsecureSkipVerify`。

## 轮换语义

| 状态 | 行为 |
|---|---|
| 文件内容未变 | Poll 返回未变化，不增加 version、attempt 或回调 |
| 新材料有效 | 完整构建新 `tls.Config` 后原子发布，新握手/新 Dial 使用新版本 |
| 新材料无效 | 保留当前配置，增加 failure 和 `LastError`，后续周期可继续重试 |
| 已有 TLS 连接 | 保持握手时配置，继续完成 RPC，不因轮换主动关闭 |
| 新客户端身份 | 旧 Client CA 签发的新客户端证书无法通过轮换后的 mTLS Server |
| 固定配置 API | `WithTLSConfig` 继续可用，`WithTLSConfigSource` 可动态提供快照 |

## 本地验证

Go 版本：`go1.23.12 linux/amd64`。

| 检查 | 结果 |
|---|---|
| `make check` | 通过：格式、生成代码、vet、单元测试和构建 |
| `make race` | 通过：全部包 `go test -race ./...` |
| `golangci-lint run` | 通过 |
| `make fault-test` | 通过：9 个确定性故障/恢复场景 |
| `make benchmark-smoke` | 通过：plaintext、TLS、mTLS 全部 0 错误 |
| `make docker-test` | 通过：发现 RPC 和 Prometheus 健康 |
| `make docker-tls-test` | 通过：双实例发现 mTLS 与无证书拒绝 |
| `make docker-tls-rotation-test` | 通过：在线材料轮换、退役身份拒绝、失败回退和 reload 指标 |

`make benchmark-smoke` 的连接建立样本为 plaintext P50 `158us`、TLS P50 `4.13ms`、mTLS P50 `4.46ms`，10 个样本均为 0 失败。该数据只用于证明轮换改动后 TLS 链路仍可运行，不作为本阶段性能阈值。

## Docker 轮换验收

`make docker-tls-rotation-test` 完成以下真实链路：

1. 启动 etcd、两个轮询间隔为 `200ms` 的 mTLS Echo Server 和 Prometheus。
2. 使用初始 CA、服务端证书和客户端身份通过服务发现完成 RPC。
3. 一次性替换服务端证书、私钥、Client CA、客户端根 CA 和客户端证书。
4. 轮询新客户端调用，直到新 CA 和新身份通过新握手。
5. 使用新根 CA 验证服务端，但提交旧 CA 签发的客户端证书，确认新连接被拒绝。
6. 写入损坏的服务端证书，等待轮询后确认 last-known-good 继续完成 RPC。
7. 检查 reload success/failure/version 指标，删除全部临时私钥和 Compose 资源。

## 验收清单

- [x] 服务端证书和私钥轮换后，新握手使用新证书。
- [x] mTLS Client CA 轮换后，旧 CA 签发的客户端无法建立新连接。
- [x] 客户端根 CA 和客户端身份轮换后，新 Dial 使用新快照。
- [x] 已有连接在轮换后继续完成 RPC，不被热加载改写或主动中断。
- [x] 无效材料不覆盖 last-known-good，failure 和最近错误可观察。
- [x] 内容未变不发布无意义版本，强制 Reload 仍可显式刷新。
- [x] 并发快照、轮询和回调通过 race detector。
- [x] 轮换指标标签有界，生产路径不允许 `InsecureSkipVerify`。
- [x] Docker Compose 真实双实例发现和 mTLS 轮换通过。
- [ ] GitHub Actions 对最终提交全部通过（推送后回填 run 链接）。

## 已知限制

- 轮询基于完整文件内容签名，适合普通证书文件挂载或原子 rename；没有内置 ACME、Vault/KMS 接入、双 CA 灰度或旧连接强制排空。
- 不校验证书吊销状态（OCSP/CRL），也不提供证书透明日志策略。
- 文件短暂处于新旧材料混合状态时该轮 reload 会失败，需依赖后续轮询或运维原子发布；这是保留 last-known-good 的安全退化，不是事务性文件系统保证。
- TLS reload 只覆盖 RPC 通道，etcd 和 `/metrics` HTTP 端点仍使用各自配置。
- 本地 benchmark 仍为同机同进程测试，轮换验收不等价于跨主机滚动发布或 KMS 审计。

## 下一阶段依赖

后续若引入 OpenTelemetry、动态策略控制面或生产级证书签发，需要分别冻结 telemetry schema、控制面 API 和信任根生命周期。证书轮换可以先作为 OTLP/KMS 接入的文件适配器，但不能把私钥读取权限扩散到 RPC 业务包。
