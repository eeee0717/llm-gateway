# llm-gateway

OpenAI 兼容的多供应商 LLM 网关，Go 单体服务。当前里程碑：**M0**（目录已搭好，尚无代码）。

## 范围与里程碑

功能广度不是目标，项目在三处做深：流式计费一致性、多实例 token 级限流、流式故障转移。每个里程碑以可验证的断言收口（测试或压测输出），而不是以「模块写完」收口；数字实测出来之后才能写进 README。

| 里程碑 | 内容 | 收口断言 |
|---|---|---|
| M0 walking skeleton | 单上游；`POST /v1/chat/completions` 流式与非流式转发；网关 Key 鉴权；mock 上游 | 下游断开后 mock 上游记录到请求被取消；流式与非流式都采集到 usage |
| M1 多渠道与持久化 | 数据库存 Key 摘要、渠道、用量；按模型路由；`GET /v1/models`；docker compose 一键启动依赖服务 | 双渠道按权重分流，偏差在阈值内 |
| M2 计量与限流 | Redis：RPM/TPM 限流（Lua 原子执行）、按 `max_tokens` 预扣、结束后按实际 usage 结算；用量事件异步落库，按 request id 幂等，定时对账 | 含随机断连的压测后，账本与 mock 上游的真实 token 数一致；多实例下限流偏差在阈值内 |
| M3 可靠性 | 首个事件之前失败就切换渠道；按渠道熔断，半开探测；流空闲超时 | 注入上游故障后，下游成功率达标 |
| M4 可观测与压测 | OpenTelemetry、Prometheus/Grafana、k6 脚本入库 | 网关自身额外增加的 P99 延迟；单实例承载并发流时的内存 |

阈值在进入对应里程碑时与人确定。

## 目录

| 路径 | 职责 |
|---|---|
| `cmd/gateway` | 网关入口：加载配置、日志、优雅退出 |
| `cmd/mockupstream` | 可独立运行的 mock 上游，用于手动调试和压测 |
| `internal/server` | 装配路由、中间件与 `http.Server` |
| `internal/relay` | 转发核心：请求改写、SSE 透传、用量采集；每次转发产出一条结果记录 |
| `internal/sse` | 按事件读取 SSE，保留原始字节以便原样转发 |
| `internal/openai` | 用到的 OpenAI 协议子集与错误响应格式 |
| `internal/auth` | 网关 Key 校验 |
| `internal/requestid` | 请求 ID：生成、写入响应头、通过 context 传递 |
| `internal/config` | 配置加载与校验 |
| `internal/mockupstream` | mock 上游实现：确定性 usage、延迟、故障注入、中途断开；测试和 `cmd/mockupstream` 共用 |
| `docs/adr` | 难以逆转的决策，文件名 `NNNN-标题.md` |

依赖方向是单向的：`cmd → server → 业务包`。M1 起新增的能力（渠道、限流、计费、熔断）各自新建一个 `internal/` 包。写了代码之后，包的职责以包注释为准。

## 设计约束

- **北向协议**：OpenAI Chat Completions。请求体只解析路由和计费要用的字段，其余字段原样透传；错误响应统一为 `{"error": {"message", "type", "code"}}`。
- **流式 usage**：上游默认不返回 usage，转发时强制带上 `stream_options.include_usage: true`。如果调用方自己没要求，就把那个只含 usage 的事件从下游流里剥掉，让下游看到的流和直连上游时一致。
- **流中途出错**：响应头已经发出后上游再出错，先发一个 `data: {"error": {...}}` 事件再结束。OpenAI 官方 SDK 收到这个事件会抛异常，调用方就能知道回答被截断了。
- **取消传播**：上游请求从下游请求的 context 派生，下游断开时上游请求随之取消。写结果记录时用 `context.WithoutCancel`，保证下游断开后照样能落账。
- **流式刷新**：包装 `http.ResponseWriter` 的中间件要实现 `Unwrap()`，否则 `http.ResponseController.Flush` 穿不过去。访问上游的 `Transport` 要调大 `MaxIdleConnsPerHost`，默认值只有 2。
- **密钥隔离**：发往上游的只有渠道密钥。上游返回 401/403 时统一转成 502 `upstream_auth_failed`，因为原始响应体里可能带有密钥片段；网关自己生成的错误信息里也不带上游地址。
- **测试走真实 HTTP**：用 `httptest` 起网关，用 `internal/mockupstream` 起假上游，断言落在调用方能观测到的行为上，比如响应内容和 mock 的计数。

## 待定决策

以下几项遇到时先问人，拍板后在 `docs/adr/` 写一篇 ADR：

- HTTP 层：`net/http` 还是 Gin
- 数据库：MySQL 还是 PostgreSQL
- 用量事件的传递：Kafka 还是 Redis Streams
- 配置来源：环境变量还是配置文件

## 约定

- 工具链由 `mise.toml` 固定，第一次进入目录时执行 `mise install`。
- 注释和文档用中文，标识符用英文。
- 依赖以标准库优先，每新增一个依赖都要在 ADR 或 PR 描述里写明理由。
- 完成标准：`gofmt -l .` 没有输出，`go vet ./...` 和 `go test -race ./...` 都通过，并且本次改动涉及的里程碑断言有测试覆盖。
