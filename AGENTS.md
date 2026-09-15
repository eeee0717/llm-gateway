# llm-gateway

OpenAI 兼容的 LLM 网关，Go 单体服务：调用方凭 API Key 调用多个上游的模型，网关负责鉴权、限流和转发，并按用量从余额中扣费。四个里程碑都已完成，两个收口数字见 `README.md`。

术语以 `CONTEXT.md` 为准，代码、文档、注释里的叫法都跟它一致；出现新的领域术语就补进去，那里只收术语、不写实现。

## 范围

小而精：非测试 Go 代码预算约 3000 行，功能超出预算就砍掉，不往后拖。深度集中在 SSE 流式转发、取消传播、并发预扣的原子性、鉴权缓存的一致性和限流算法，代码和 `docs/notes/` 要把这几处讲透。

- 做：`POST /v1/chat/completions`（流式与非流式）、`GET /v1/models`、API Key 鉴权、按 Key 的 RPM 限流、预扣与结算、多个上游按模型路由（每个模型固定走一个上游）、管理接口、结构化日志、mock 上游、压测程序。
- 不做：故障转移与熔断、Kafka、OpenTelemetry / Prometheus / Grafana、管理后台 UI、非 OpenAI 协议、K8s、模型别名。

## 里程碑

每个里程碑以可验证的断言收口（测试或压测输出），而不是以「模块写完」收口。先把收口断言写成一个失败的测试，再去实现。

| 里程碑 | 内容 | 收口断言 |
|---|---|---|
| M1 转发 | 配置加载；多个上游按模型路由；流式与非流式透传；采集用量；中途断开时取消上游请求；`GET /v1/models`；mock 上游（输出确定、可配延迟、可注入故障、可模拟上游中途出错）。不含鉴权和计费 | 中途断开后 mock 上游观测到请求被取消，并得到估算用量；上游中途出错时调用方收到错误；流式与非流式都采集到上游报告的用量；调用方没要求 usage 时，流里没有只含 usage 的事件 |
| M2 Key 与计费 | docker compose（MySQL + Redis）、goose 迁移、管理接口、鉴权（不接缓存）、预扣与结算、用量记录 | 两个网关实例共用一套 MySQL，1000 个并发请求打同一个 API Key，余额零超扣 |
| M3 Redis | 鉴权缓存、令牌桶限流 | Key 被禁用后立即失效；限流放行的请求总数正确 |
| M4 交付 | 首 token 延迟压测、Dockerfile、compose 一键启动全套服务、CI、README | 测出经过网关比直连多出的首 token 延迟 P99 |

README 里的数字只写实测结果：M2 的余额零超扣断言和 M4 的首 token 延迟 P99 额外开销。

## 选型

| 用途 | 选择 |
|---|---|
| HTTP | Gin |
| 数据库 | MySQL 8.4 + GORM，驱动 go-sql-driver；连接串的必备开关由 `config.NormalizeDSN` 补齐，见 ADR-0006 |
| 迁移 | goose：SQL 文件放 `migrations/`，embed 进二进制，由 `gateway migrate` 执行 |
| Redis | go-redis v9 |
| 并发合并 | `golang.org/x/sync/singleflight`：鉴权缓存的回源合并成一次，见 ADR-0005 |
| 日志 | `log/slog`；Gin 的访问日志由自写中间件接到 slog |
| 配置 | 一个 YAML 文件，用 `go.yaml.in/yaml/v3` 解析（`gopkg.in/yaml.v3` 已归档）。上游密钥和管理员密钥只从环境变量读取，YAML 里只写变量名，如 `key_env: DEEPSEEK_API_KEY` |
| 测试 | testify 的 `require`；静态检查用 golangci-lint |
| 交付 | 多阶段构建 + distroless 镜像；本地依赖由 OrbStack + docker compose 提供，测试也连这套服务；CI 用 GitHub Actions，MySQL 和 Redis 用 service container |

依赖以这张表为限，其余用标准库；需要表外的依赖时先问人，定下来后写 ADR。

## 目录与分层

```
cmd/
  gateway/        子命令 serve / migrate；所有依赖在这里组装
  mockupstream/   独立运行的 mock 上游
  loadtest/       首 token 延迟压测
internal/
  server/         Gin 引擎、公共中间件；业务端口与管理端口两个监听
  relay/          /v1/chat/completions 与 /v1/models：模型路由、请求改写、SSE 透传、用量解析与估算
  billing/        费用计算、预扣、结算、用量记录
  apikey/         Key 生成与哈希、鉴权中间件、鉴权缓存、禁用
  ratelimit/      令牌桶限流（Redis Lua）；中间件接一个 Caller 函数，不 import apikey
  admin/          管理接口
  requestid/      请求 ID（relay 与 admin 共用，单独成包以免循环依赖）
  config/         配置加载与校验
  sse/            按事件读取 SSE，保留原始字节以便原样转发
  openai/         用到的 OpenAI 协议子集与错误响应
  mockupstream/   mock 上游实现，测试与 cmd/mockupstream 共用
  testdb/         测试用的数据库连接：连 compose 起的 MySQL，首次使用时执行迁移
  testredis/      测试用的 Redis 连接：连 compose 起的那套
migrations/       goose SQL 迁移，embed 进二进制
scripts/          实验脚本，比如用量记录那条索引的实测依据
compose.yaml      本地依赖：MySQL 与 Redis
docs/adr/         难以逆转的决策，文件名 NNNN-英文短名.md
docs/notes/       每个功能一页说明
```

- 依赖方向单向：`cmd → server → 业务包`。
- 业务包内部按 `handler.go`（HTTP 处理）/ `service.go`（业务逻辑）/ `store.go`（数据访问）分文件，用不到的层不建。
- 接口由使用方定义：`relay` 自己声明只含预扣和结算的小接口，由 `cmd/gateway` 注入 billing 的实现，relay 不 import billing。
- 包写出代码后，职责以包注释为准。

一次聊天请求依次经过：鉴权（apikey，先查鉴权缓存）→ 限流（ratelimit）→ 预扣（billing，MySQL 条件更新）→ 转发（relay）→ 结算（billing，与用量记录同一事务）。改动计费、余额、鉴权缓存、限流或 API Key 存储之前，先读 `docs/adr/` 里对应的 ADR：0001 预扣与结算，0002 余额与 Redis 的分工，0003 API Key 哈希，0004 按 Key 的令牌桶，0005 鉴权缓存的回源合并与 TTL 偏移，0006 换成 MySQL 之后哪些写法必须改。

## 设计约束

- **对外协议**：OpenAI Chat Completions。请求体只解析路由和计费要用的字段，其余字段原样透传。错误响应统一为 `{"error": {"message", "type", "code"}}`；余额不够预扣返回 429 `insufficient_quota`，被限流返回 429 `rate_limit_exceeded`。
- **流式用量**：上游默认不在流里报告用量，转发时强制带上 `stream_options.include_usage: true`。调用方自己没要求时，把最后那个只含 usage 的事件从流里剥掉，其余事件原样转发（上游在其余事件里附带的 `"usage": null` 不改写），让调用方看到的流和直连上游时等价。
- **上游中途出错**：流式响应的响应头已经发给调用方之后上游再出错（包括没发 `[DONE]` 就断开），先发一个 `data: {"error": {...}}` 事件再结束。OpenAI 官方 SDK 收到这个事件会抛异常，调用方就能知道回答被截断了。非流式响应还没开始写，直接返回 502。
- **取消传播**：上游请求从 `c.Request.Context()` 派生，调用方中途断开时上游请求随之取消。`*gin.Context` 虽然实现了 `context.Context`，但引擎没开 `ContextWithFallback` 时它的 `Done()` 返回 nil，传它就取消不了。结算用 `context.WithoutCancel`，保证中途断开后照样能结算。
- **流式刷新**：每写完一个 SSE 事件就调用 `c.Writer.Flush()`。包装 `gin.ResponseWriter` 的中间件要保留 `Flush()`，并自己实现 `Unwrap()`（它不在 `gin.ResponseWriter` 接口里，嵌入接口得不到）；访问日志直接读 `c.Writer.Status()` 和 `Size()`，不用包装。业务端口的 `http.Server` 不设 `WriteTimeout`，访问上游的 `http.Client` 不设 `Timeout`，两者都会截断长流；等上游响应头用 `Transport.ResponseHeaderTimeout`，并调大 `MaxIdleConnsPerHost`（默认只有 2）。
- **密钥隔离**：发往上游的只有上游密钥。上游返回 401/403 时统一转成 502 `upstream_auth_failed`，因为原始响应体里可能带有密钥片段；网关自己生成的错误信息里也不带上游地址。日志里的 API Key 只记 ID。
- **测试走真实 HTTP**：用 `httptest` 起网关，用 `internal/mockupstream` 起上游，断言落在调用方能观测到的行为上，比如响应内容和 mock 的计数。M2 起测试连 docker compose 起的 MySQL 和 Redis。

## 约定

- 工具链（Go、golangci-lint）由 `mise.toml` 固定，第一次进入目录时执行 `mise install`。
- 注释、文档和命令行帮助用中文；标识符、日志、错误信息和 API 响应用英文。代码写得直白、地道，关键设计在注释或 notes 里写明理由。
- 仓库公开：文档只讲技术本身，标题措辞保持中性。
- 每完成一个功能，在 `docs/notes/` 写一页，文件名用英文短名，分「问题 / 方案 / 取舍 / 延伸问题」四节。
- 本文没覆盖到的架构或选型问题先问人，定下来后在 `docs/adr/` 写一篇 ADR，几句话说清背景、决定和理由；业务细节按常规做法自己定，汇报时列出来。

## 完成标准

每个里程碑结束时：

1. `gofmt -l .` 无输出；`go vet ./...`、`golangci-lint run`、`go test -race ./...` 全部通过。
2. 收口断言有测试覆盖，`docs/notes/` 里有对应的说明。
3. 汇报非测试代码行数和 3000 行预算的余量，统一用这条命令计数（不含空行和整行注释）：
   `find cmd internal migrations -name '*.go' ! -name '*_test.go' -exec cat {} + | grep -cvE '^[[:space:]]*(//.*)?$'`
