# Mini-RPC 二进制协议 v1

本文档描述 Mini-RPC v1 的 TCP 字节流分帧格式。实现位于 internal/protocol。

## 设计目标

- 在连续 TCP 字节流中可靠识别帧边界。
- 将响应、取消和心跳关联到连接内唯一的 RequestID。
- 通过固定上限和非负长度校验阻止恶意超长帧。
- 允许 Metadata 携带服务路由、Deadline、Trace 和重试信息。
- 对同一 Metadata 集合生成确定性的字节序列。
- 为后续连接池、多路复用和错误映射提供稳定字节边界。

## 字节序

所有多字节整数使用大端序（network byte order）。固定头部编码在 Metadata 和 Body 之前。

## 固定头部

固定头部长度为 22 字节：

| 偏移 | 长度 | 字段 | v1 含义 |
|---:|---:|---|---|
| 0 | 2 | Magic | 固定为 0x4D52，ASCII 为 MR |
| 2 | 1 | Version | 固定为 1 |
| 3 | 1 | Type | Request=1、Response=2、Cancel=3、Heartbeat=4 |
| 4 | 2 | Flags | 压缩、错误、单向请求等标志 |
| 6 | 4 | HeaderLength | Metadata 编码后的字节数 |
| 10 | 4 | BodyLength | Protobuf Body 的字节数 |
| 14 | 8 | RequestID | 单连接内唯一，请求与响应使用相同值 |
| 22 | HeaderLength | Metadata | 长度前缀键值对 |
| 22+HeaderLength | BodyLength | Body | Protobuf 请求或响应 |

## Flags

| 位 | 名称 | 含义 |
|---:|---|---|
| 0 | Compressed | Body 使用后续约定的压缩算法 |
| 1 | Error | 当前响应表示 RPC 失败 |
| 2 | OneWay | 请求不期望响应 |

其他位在 v1 中保留。编码和读取发现未知位时必须返回 ErrInvalidFlags。压缩算法本身不在本阶段定义，设置 Compressed 后的 Body 处理由后续压缩功能补充。

## Metadata 编码

Metadata 不使用 Protobuf map 编码，而是采用确定性的长度前缀序列：

~~~text
repeat:
  key_length:   uint32 big endian
  key:          key_length bytes
  value_length: uint32 big endian
  value:        value_length bytes
~~~

规则：

- 编码前按 key 的字典序排序，保证等价 map 产生相同字节。
- key 必须非空。
- value 可以为空。
- 同一帧内禁止重复 key，避免解码覆盖和代理行为不确定。
- HeaderLength=0 表示没有 Metadata。
- HeaderLength 必须完整覆盖整数个键值对，否则返回 ErrMalformedMetadata。

保留 key：

- service
- method
- deadline_unix_nano
- trace_id
- span_id
- content_type
- retry_attempt
- error_code

error_code 使用 Metadata 携带响应错误码，不改变固定头部布局。

## 上限与资源保护

| 项目 | 默认值 | 配置规则 |
|---|---:|---|
| Header | 64 KiB | v1 硬上限，自定义 codec 只能设置更小值 |
| Body | 8 MiB | 可配置更小或更大值，但不得超过平台可分配范围 |
| 单连接并发请求 | 1024 | 由后续 Transport 实现 |

读取端在看到 HeaderLength 或 BodyLength 后、分配负载内存前完成上限校验。非法 Magic、Version、Type、Flags、HeaderLength 或 BodyLength 都返回带分类的协议错误，Transport 应关闭连接。

## 读取边界

- 两次帧之间的干净 EOF 返回 io.EOF，表示连接正常结束。
- 固定头部读取到任意部分字节后 EOF 返回 ErrTruncatedFrame。
- Metadata 或 Body 未读满声明的长度返回 ErrTruncatedFrame。
- 一次 Read 只读取一个完整帧，不消耗下一帧。
- 一次 Read 必须使用 io.ReadFull 语义处理拆包。
- 连续多个帧由调用方循环 Read。

## 写入边界

- Codec 每次 Encode 都分配独立缓冲区，不返回内部复用缓冲。
- Codec 构造后不可变，并发生成和解析不共享可变状态。
- Write 使用循环处理短写；writer 返回 0 且无错误时返回 io.ErrShortWrite。
- Body 在编码时复制到输出缓冲区，修改原文不会改变已经编码的字节。

## Go API

~~~go
codec := protocol.DefaultCodec
encoded, err := codec.Encode(frame)
err = codec.Write(writer, frame)
frame, err = codec.Read(reader)
~~~

自定义限制：

~~~go
codec, err := protocol.NewCodec(protocol.Limits{
    MaxHeaderSize: 32 << 10,
    MaxBodySize:   1 << 20,
})
~~~

便捷函数 ReadFrame 和 WriteFrame 使用 DefaultCodec。

## 示例十六进制帧

### 空 Metadata、空 Body、RequestID=42

~~~text
4d 52  01  01  00 00  00 00 00 00  00 00 00 00  00 00 00 00 00 00 00 2a
|magic|ver|typ|flags|header len|  body len   |       request id          |
~~~

连续十六进制：

~~~text
4d52010100000000000000000000000000000000002a
~~~

### method=Echo、service=echo-1、RequestID=42

HeaderLength 为 39 字节（0x00000027）。Metadata 按 method、service 排序：

~~~text
头部：
4d 52 01 01 00 00 00 00 00 27 00 00 00 00 00 00 00 00 00 00 00 2a

Metadata：
00 00 00 06  6d 65 74 68 6f 64  00 00 00 04  45 63 68 6f
00 00 00 07  73 65 72 76 69 63 65  00 00 00 06  65 63 68 6f 2d 31
~~~

## 协议错误

| 错误 | 触发条件 |
|---|---|
| ErrInvalidMagic | 流不是 Mini-RPC，或 Magic 被破坏 |
| ErrUnsupportedVersion | Version 不是 1 |
| ErrInvalidFrameType | Type 不在 1 到 4 范围内 |
| ErrInvalidFlags | 设置了 v1 未定义标志位 |
| ErrInvalidLimits | Header 限制超过硬上限或 Body 限制不可分配 |
| ErrHeaderTooLarge | HeaderLength 或编码后 Header 超过限制 |
| ErrBodyTooLarge | BodyLength 或编码 Body 超过限制 |
| ErrTruncatedFrame | 帧在读取过程中提前结束 |
| ErrMalformedMetadata | Metadata 长度前缀或内容不完整 |
| ErrEmptyMetadataKey | key 为空 |
| ErrDuplicateMetadataKey | 同一帧内重复 key |
| Error | 携带 Kind 和 Detail，支持 errors.Is/errors.As |

## 测试矩阵

- 编解码等价、固定头部字节序和 Metadata 排序。
- 单字节拆包、连续多帧、干净 EOF 和截断帧。
- 非法 Magic、Version、Type、Flags 和超限长度。
- Metadata 长度前缀截断、空 key 和重复 key。
- 自定义限制和短写处理。
- 64 个 goroutine 并发 Encode，结合 race detector 验证不共享可变缓冲。
- 所有测试只使用内存 reader/writer，不依赖真实网络。
