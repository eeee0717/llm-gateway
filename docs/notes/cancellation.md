# 取消传播

## 问题

调用方随时可能断开：关掉页面、点了停止、客户端超时。这时上游还在生成，每多生成一个 token 都要花钱。网关要做到：调用方一断开，上游请求立刻取消；同时这次请求照样要结算，不能因为请求被取消就漏记。

## 方案

**一条 context 链贯穿到上游。**

1. `net/http` 服务端为每个请求创建一个 context。HTTP/1.1 下，读完请求体后，服务端会在后台继续读这条连接；调用方一断开，后台读就读到 EOF，这个 context 随即被取消。
2. relay 用 `c.Request.Context()` 创建上游请求（`http.NewRequestWithContext`）。
3. context 被取消后，`http.Transport` 关闭到上游的连接；上游那边的请求 context 也跟着被取消，mock 上游据此记一次"被取消"。

**不能拿 `*gin.Context` 当 context 用。** 它虽然实现了 `context.Context` 接口，但 Gin 引擎默认没开 `ContextWithFallback`，这时它的 `Done()` 返回 nil，永远不会被取消。把它传进去，代码能编译、平时也能跑，只是调用方断开后上游请求不会被取消。

**区分两种失败。** 发请求或读响应出错时，relay 先看 `ctx.Err()`：不为 nil，说明是调用方断开引起的取消，不算上游出错，也不写错误响应（写了也送不到）；否则才是上游的问题。这个区分决定了按哪条规则结算，见[用量采集与估算](usage-metering.md)。

**结算不跟着取消。** M2 起结算要写数据库，如果沿用请求的 context，调用方一断开结算也会失败。所以结算用 `context.WithoutCancel(ctx)`：保留 context 里的值（请求 ID），去掉取消信号。

**测试直接观察上游。** `TestCallerDisconnectMidStreamCancelsUpstreamAndEstimatesUsage` 让调用方收到第一个 token 就取消请求，然后断言两件事：mock 上游的 `Canceled()` 变成 1，结算收到的是估算用量。

## 取舍

- **中途断开按估算收费。** 估算只按已经转发的内容算，偏差有两个方向：上游在收到取消之前可能多生成了几个 token，这部分没有转发，也就没有计入，网关少收；另一方面，"已转发"指的是已经写进网关的发送缓冲区，调用方断开前不一定全部收到，调用方多付。
- **兜底靠调用方断开。** 访问上游的 `http.Client` 不设整体超时（它会截断长流），只设了 `ResponseHeaderTimeout`。上游迟迟不响应时，最终由调用方的超时触发断开，再沿这条链取消上游请求。
- **优雅退出有上限，超时后主动取消。** 收到 SIGTERM 后，`http.Server.Shutdown` 会等进行中的请求处理完（包括结算），但最多等 30 秒。还没结束的请求会被取消：所有请求的 context 都从 `http.Server.BaseContext` 派生，取消它等于让这些请求走"调用方中途断开"那条路，按估算用量结算，之后网关再多等 5 秒让它们把账写完。这样退出时不会留下没有结算的预扣。

## 延伸问题

- `context.WithoutCancel` 和 `context.Background()` 有什么区别？前者保留父 context 里的值（请求 ID 等），只去掉取消信号和截止时间。
- HTTP/2 下服务端怎么感知调用方断开？客户端发 RST_STREAM，服务端同样会取消这个请求的 context，不需要后台读。
- 退出时被取消的请求，调用方收到的是一个截断的流（流式）或者连接中断（非流式）。要让调用方知道是网关在重启，可以在取消之前先发一个错误事件，但那需要区分"调用方断开"和"网关退出"两种取消。
- 为什么 `ResponseHeaderTimeout` 设成 10 分钟这么长？非流式请求要等上游生成完整个回答才会返回响应头。
