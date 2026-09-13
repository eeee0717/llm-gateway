# llm-gateway

OpenAI 兼容的 LLM 网关。调用方拿一个 API Key 调用多个上游的模型，网关负责鉴权、限流、转发，并按实际用量从余额里扣费。

- `POST /v1/chat/completions`，流式与非流式；SSE 事件原样透传，调用方看到的流和直连上游时等价
- `GET /v1/models`
- API Key 鉴权：只存哈希，鉴权结果进 Redis 缓存，禁用后立即失效
- 按 Key 限流：Redis 里的令牌桶，多个网关实例共享同一个桶
- 预扣与结算：转发前按最大可能的费用预扣，请求结束后按实际用量多退少补
- 多个上游按模型路由，每个模型固定走一个上游
- 管理接口单开一个端口：建 Key、充值、改额度、禁用、查余额

单体 Go 服务，非测试代码约 2200 行。技术栈：Gin、PostgreSQL（GORM + pgx）、Redis（go-redis）、goose 迁移、`log/slog`。

## 跑起来

不需要任何真实的上游密钥——一键起来的这套里带了一个 mock 上游。

```sh
docker compose --profile full up -d --build
```

起 PostgreSQL、Redis、mock 上游和网关（业务端口 8080，管理端口 8081），迁移由一个跑完就退出的容器执行，网关等它成功之后才启动。等到 `docker compose logs gateway` 里出现 `listening` 就可以打了。

建一个 Key 并充值 100 元：

```sh
curl -s -X POST localhost:8081/admin/keys \
  -H 'Authorization: Bearer local-admin-key' -H 'Content-Type: application/json' \
  -d '{"name":"demo"}'
# {"id":1,"name":"demo","key":"sk-...","balance_micro":0,...}   key 只在这里出现一次

curl -s -X POST localhost:8081/admin/keys/1/credit \
  -H 'Authorization: Bearer local-admin-key' -H 'Content-Type: application/json' \
  -d '{"amount_micro":100000000}'
```

拿它调模型，OpenAI 的 SDK 直接把 base_url 指过来就能用：

```sh
curl -N -X POST localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-..." -H 'Content-Type: application/json' \
  -d '{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

要接真实上游的话，把 `config.compose.yaml` 里的上游换成自己的，地址和密钥都只写环境变量名（`base_url_env` / `key_env`），值从环境里读。

## 实测数字

### 余额零超扣

两个网关实例共用一套 PostgreSQL，1000 个并发请求打同一个 API Key，余额只够其中一部分成功。断言（`TestTwoInstancesDoNotOverspendOneKey`）：

- 每个请求要么成功、要么因为余额不够被 429 拒绝，没有第三种结果
- **余额不小于零**
- 账目守恒：`充值额 = 余额 + 所有用量记录的费用之和`
- 每个成功的请求留下且只留下一条用量记录

靠的是一条带条件的 UPDATE（`WHERE 余额 >= 预扣额 AND NOT disabled`）用 `RowsAffected` 判断成败，同一个 Key 的并发预扣由行锁排队，实例之间不需要额外协调。详见 [ADR-0001](docs/adr/0001-reserve-then-settle.md)。

### 首 token 延迟

`cmd/loadtest` 对直连上游和经过网关两端交替打同样的流式请求，比较第一个 SSE 事件到达的时间。上游是首 token 固定 300 毫秒的 mock，500 对请求、20 并发：

```
                N      min      p50      p90      p99      max
      direct  500  300.1ms  300.6ms  301.5ms  303.2ms  303.3ms
     gateway  500  302.3ms  308.1ms  319.6ms  334.8ms  347.3ms
  gateway 多出        +2.1ms   +7.5ms  +18.1ms  +31.6ms  +43.9ms
```

**P99 多出 31.6 毫秒**，中位数多出 7.5 毫秒。并发降到 5 时 P99 的差掉到 +18.3ms，而 min 几乎不动——固有开销约 2 毫秒，尾部是同一个 Key 的预扣在行锁上排队。测量方法和这个数字的读法见 [压测说明](docs/notes/loadtest.md)。

数字是在一台开发机上测的，网关、数据库、Redis 和压测程序共用同一份 CPU。

对着真实上游也跑了三轮对照，差值被上游自己的抖动完全盖住——同样的配置，P99 的差在 -3.1 秒和 +1.1 秒之间摆。这正是主口径用 mock 的原因。

### 吞吐与瓶颈

同一个压测程序换个模式（`-mode throughput`），mock 不加延迟，50 并发各打 20 秒。这里的 p50/p99 是整个请求读完的耗时，不是上面那个首 token 延迟：

| 被压的一端 | QPS | p50 | p99 |
|---|---|---|---|
| mock 上游（对照） | 60,763 | 0.7ms | 2.5ms |
| 网关，20 个 Key | **3,192** | 14.3ms | 36.6ms |
| 网关，全部打同一个 Key | 565 | 80.4ms | 222.9ms |

瓶颈是 PostgreSQL——压测期间 PG 容器 CPU 占 95.5%，Redis 只有 13%；一个请求要写两次库。连接池从 20 放到 60 只换来 15% 的吞吐，所以那个限制没有卡住谁。**同一个 Key 和 20 个 Key 差 5.7 倍**，这是预扣行锁的直接代价，也是上面 P99 那条尾巴的实证。

## 一次请求经过什么

```
鉴权 → 限流 → 预扣 → 转发 → 结算
```

- **鉴权**（`internal/apikey`）：SHA-256 查 Redis 缓存，没有就回源 PostgreSQL 再写回；查不到的 Key 也记 30 秒，挡住拿随机 Key 反复打的情况
- **限流**（`internal/ratelimit`）：一段 Lua 脚本在 Redis 里一次完成令牌的补充和扣减，多实例共享一个桶
- **预扣**（`internal/billing`）：按 `输出上限 × 输出单价 + prompt token × 输入单价` 扣下最大可能的费用
- **转发**（`internal/relay`）：流式响应逐个事件转发并 flush；调用方中途断开时上游请求随之取消
- **结算**（`internal/billing`）：按上游报告的用量（或中途断开时的估算用量）多退少补，和用量记录同一个事务提交

余额只有 PostgreSQL 一份，Redis 里的东西丢光了也只是慢一点、不会算错账，所以 Redis 故障时网关照常工作（[ADR-0002](docs/adr/0002-balance-in-postgres-only.md)）。

至于"慢一点"是多少，实测过：把鉴权缓存摘掉，纯鉴权路径从 0.204ms 变成 0.228ms，**只差 0.02 毫秒**——PostgreSQL 那条查询走唯一索引、内存命中，本来就只要 0.011 毫秒。这层缓存买到的不是当下的速度，而是把读流量从最难水平扩的那一层挪走（[auth-cache](docs/notes/auth-cache.md)）。

## 本地开发

工具链由 `mise.toml` 固定：

```sh
mise install
docker compose up -d        # 只起 PostgreSQL 和 Redis，测试连的就是这一套
go test -race ./...
```

测试走真实 HTTP、真实数据库和真实 Redis，断言落在调用方能观测到的行为上。

```sh
go run ./cmd/gateway migrate -config config.yaml   # 迁移
go run ./cmd/gateway serve   -config config.yaml   # 启动
go run ./cmd/mockupstream                          # 可配延迟和故障的 mock 上游
go run ./cmd/loadtest -direct ... -gateway ...     # 首 token 延迟压测
```

配置文件的样子见 [`config.example.yaml`](config.example.yaml)。上游密钥和管理员密钥只从环境变量读取，YAML 里只写变量名。

## 设计文档

难以逆转的决定写成 ADR，每个功能一页说明：

| ADR | |
|---|---|
| [0001](docs/adr/0001-reserve-then-settle.md) | 预扣与结算 |
| [0002](docs/adr/0002-balance-in-postgres-only.md) | 余额只存 PostgreSQL，Redis 只做缓存和限流 |
| [0003](docs/adr/0003-api-key-sha256.md) | API Key 用 SHA-256 存哈希 |
| [0004](docs/adr/0004-per-key-token-bucket.md) | 按 Key 的令牌桶，时间由网关提供 |

| 说明 | |
|---|---|
| [sse-relay](docs/notes/sse-relay.md) | SSE 转发与刷新 |
| [cancellation](docs/notes/cancellation.md) | 取消传播 |
| [usage-metering](docs/notes/usage-metering.md) | 用量采集与估算 |
| [billing](docs/notes/billing.md) | 预扣、结算与并发 |
| [api-key](docs/notes/api-key.md) | Key 的生成与存储 |
| [auth-cache](docs/notes/auth-cache.md) | 鉴权缓存的一致性 |
| [rate-limit](docs/notes/rate-limit.md) | 令牌桶限流 |
| [upstream-access](docs/notes/upstream-access.md) | 配置与上游访问 |
| [loadtest](docs/notes/loadtest.md) | 首 token 延迟压测 |

术语以 [CONTEXT.md](CONTEXT.md) 为准。
