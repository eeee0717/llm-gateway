package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/mockupstream"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/relay"
	"github.com/eeee0717/llm-gateway/internal/server"
	"github.com/eeee0717/llm-gateway/internal/sse"
)

// 消息文本是 5 个 ASCII 字符：mock 上游报告 5 个 prompt token；网关按 0.3 token/字符估算，向上取整为 2。
const helloMessages = `"messages":[{"role":"user","content":"hello"}]`

// defaultMaxTokens 是测试配置里每个模型的默认输出上限。
const defaultMaxTokens = 4096

func TestNonStreamReturnsAnswerAndReportedUsage(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{Tokens: 3}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var answer struct {
		Choices []struct {
			Message struct{ Content string }
		}
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&answer))
	require.Equal(t, "The quick brown", answer.Choices[0].Message.Content)

	requestID := resp.Header.Get("X-Request-ID")
	require.NotEmpty(t, requestID)
	require.Equal(t, relay.Result{
		RequestID: requestID,
		Model:     "mock-model",
		Usage:     openai.Usage{PromptTokens: 5, CompletionTokens: 3},
	}, gw.biller.next(t))
}

func TestStreamHidesUsageEventCallerDidNotAskFor(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{Tokens: 3}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model","stream":true,`+helloMessages+`}`)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	events := readEvents(t, resp.Body)
	require.Equal(t, "The quick brown", answerIn(t, events))
	require.Zero(t, usageEventsIn(t, events))
	require.Equal(t, "[DONE]", events[len(events)-1])

	require.Equal(t, relay.Result{
		RequestID: resp.Header.Get("X-Request-ID"),
		Model:     "mock-model",
		Usage:     openai.Usage{PromptTokens: 5, CompletionTokens: 3},
	}, gw.biller.next(t))
}

func TestStreamKeepsUsageEventCallerAskedFor(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{Tokens: 3}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model","stream":true,"stream_options":{"include_usage":true},`+helloMessages+`}`)

	events := readEvents(t, resp.Body)
	require.Equal(t, 1, usageEventsIn(t, events))
	require.Equal(t, openai.Usage{PromptTokens: 5, CompletionTokens: 3}, gw.biller.next(t).Usage)
}

// M1 收口断言：调用方中途断开后，上游请求随之取消，并按估算用量结算。
func TestCallerDisconnectMidStreamCancelsUpstreamAndEstimatesUsage(t *testing.T) {
	mock := mockupstream.New(mockupstream.Options{Tokens: 100, TokenDelay: 20 * time.Millisecond})
	gw := startGateway(t, map[string]http.Handler{"mock-model": mock})
	ctx, disconnect := context.WithCancel(t.Context())

	resp := gw.post(t, ctx, `{"model":"mock-model","stream":true,`+helloMessages+`}`)
	_, err := sse.NewReader(resp.Body).Next() // 收到第一个 token 就断开
	require.NoError(t, err)
	disconnect()

	require.Eventually(t, func() bool { return mock.Canceled() == 1 }, 5*time.Second, 10*time.Millisecond)
	result := gw.biller.next(t)
	require.True(t, result.Estimated)
	require.Equal(t, 2, result.Usage.PromptTokens)
	require.Positive(t, result.Usage.CompletionTokens) // 按已经转发的回答估算
}

func TestCallerDisconnectBeforeResponseCancelsUpstreamAndEstimatesPrompt(t *testing.T) {
	mock := mockupstream.New(mockupstream.Options{Tokens: 3, FirstDelay: 5 * time.Second})
	gw := startGateway(t, map[string]http.Handler{"mock-model": mock})
	ctx, disconnect := context.WithCancel(t.Context())
	go func() { // 等请求到了上游、上游还在生成时再断开
		for mock.Requests() == 0 && ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		disconnect()
	}()

	_, err := http.DefaultClient.Do(gw.request(t, ctx, `{"model":"mock-model",`+helloMessages+`}`))
	require.ErrorIs(t, err, context.Canceled)

	require.Eventually(t, func() bool { return mock.Canceled() == 1 }, 5*time.Second, 10*time.Millisecond)
	result := gw.biller.next(t)
	require.True(t, result.Estimated)
	require.Equal(t, openai.Usage{PromptTokens: 2, CompletionTokens: 0}, result.Usage)
}

// M1 收口断言：上游中途出错时，调用方收到错误事件，已转发的部分按估算用量结算。
func TestUpstreamFailureMidStreamSendsErrorEventAndEstimatesUsage(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{Tokens: 10, AbortAfter: 2}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model","stream":true,`+helloMessages+`}`)

	events := readEvents(t, resp.Body)
	require.Equal(t, "The quick", answerIn(t, events))
	var last openai.ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(events[len(events)-1]), &last))
	require.Equal(t, "upstream_interrupted", last.Error.Code)

	result := gw.biller.next(t)
	require.True(t, result.Estimated)
	// 已转发的 "The quick" 共 9 个 ASCII 字符，按 0.3 token/字符估算为 2.7，向上取整为 3
	require.Equal(t, openai.Usage{PromptTokens: 2, CompletionTokens: 3}, result.Usage)
}

func TestUpstreamFailureMidResponseReturns502AndChargesNothing(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{Tokens: 3, AbortAfter: 1}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	require.Equal(t, "upstream_interrupted", errorIn(t, resp).Code)
	result := gw.biller.next(t)
	require.True(t, result.Estimated)
	require.Equal(t, openai.Usage{}, result.Usage) // 调用方什么也没收到，按零用量结算
}

func TestUpstreamAuthFailureBecomes502WithoutLeakingUpstreamBody(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{FailStatus: http.StatusUnauthorized}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model","stream":true,`+helloMessages+`}`)

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), "mock upstream error") // 上游的原始响应体可能带着密钥片段
	var e openai.ErrorResponse
	require.NoError(t, json.Unmarshal(body, &e))
	require.Equal(t, "upstream_auth_failed", e.Error.Code)
	require.Equal(t, openai.Usage{}, gw.biller.next(t).Usage)
}

func TestUpstreamErrorKeepsStatusAndMessage(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"mock-model": mockupstream.New(mockupstream.Options{FailStatus: http.StatusServiceUnavailable}),
	})

	resp := gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, openai.Error{
		Message: "mock upstream error (status 503)",
		Type:    "mock_error",
		Code:    "mock_failure",
	}, errorIn(t, resp))
	require.Equal(t, openai.Usage{}, gw.biller.next(t).Usage)
}

func TestUpstreamErrorBodyBecomesUnifiedFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want openai.Error
	}{
		{
			name: "html page from a proxy",
			body: "<html>502 Bad Gateway</html>",
			want: openai.Error{
				Message: "upstream returned an unexpected response (status 502)",
				Type:    "upstream_error",
				Code:    "upstream_error",
			},
		},
		{
			name: "numeric error code",
			body: `{"error":{"message":"quota exceeded","type":"server_error","code":502}}`,
			want: openai.Error{Message: "quota exceeded", Type: "server_error"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(tc.body))
			})
			gw := startGateway(t, map[string]http.Handler{"mock-model": upstream})

			resp := gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

			require.Equal(t, http.StatusBadGateway, resp.StatusCode)
			require.Equal(t, tc.want, errorIn(t, resp))
		})
	}
}

func TestStreamRequestAnsweredWithoutSSEBecomes502(t *testing.T) {
	// 有的上游出错时照样回 200，响应体是 JSON 格式的错误
	errorWith200 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"model is overloaded","type":"server_error","code":"overloaded"}}`))
	})
	gw := startGateway(t, map[string]http.Handler{"mock-model": errorWith200})

	resp := gw.post(t, t.Context(), `{"model":"mock-model","stream":true,`+helloMessages+`}`)

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	require.Equal(t, openai.Error{Message: "model is overloaded", Type: "server_error", Code: "overloaded"}, errorIn(t, resp))
	require.Equal(t, openai.Usage{}, gw.biller.next(t).Usage)
}

func TestUpstreamStreamEndingBeforeAnyContentChargesNothing(t *testing.T) {
	// 上游回了 200 和 SSE 响应头，一个事件也没发就结束了响应
	emptyStream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	gw := startGateway(t, map[string]http.Handler{"mock-model": emptyStream})

	resp := gw.post(t, t.Context(), `{"model":"mock-model","stream":true,`+helloMessages+`}`)

	events := readEvents(t, resp.Body)
	require.Len(t, events, 1)
	var e openai.ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(events[0]), &e))
	require.Equal(t, "upstream_interrupted", e.Error.Code)
	require.Equal(t, openai.Usage{}, gw.biller.next(t).Usage)
}

// 余额不够时请求根本不转发，也就不会产生费用。
func TestRejectsRequestWhenReserveFails(t *testing.T) {
	mock := mockupstream.New(mockupstream.Options{Tokens: 3})
	gw := startGateway(t, map[string]http.Handler{"mock-model": mock})
	gw.biller.reserveOK.Store(false)

	resp := gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "insufficient_quota", errorIn(t, resp).Code)
	require.Zero(t, mock.Requests())
	require.Empty(t, gw.biller.results) // 没预扣成功就没有要结算的
}

// 调用方没指定输出上限时，按模型的默认值预扣，并把它写进发给上游的请求，免得上游生成得比预扣的假设还多。
func TestFillsInTheModelDefaultOutputLimit(t *testing.T) {
	upstream := &recordingUpstream{bodies: make(chan []byte, 1)}
	gw := startGateway(t, map[string]http.Handler{"mock-model": upstream})

	gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

	require.Equal(t, defaultMaxTokens, maxTokensIn(t, <-upstream.bodies))
	reservation := gw.biller.nextReservation(t)
	require.Equal(t, "mock-model", reservation.Model)
	require.Equal(t, defaultMaxTokens, reservation.MaxOutputTokens)
	require.Equal(t, 2, reservation.PromptTokens) // "hello" 估算出 2 个 token
}

// 调用方自己指定了输出上限就按它预扣，请求体不改。
func TestUsesTheCallerOutputLimitForReserve(t *testing.T) {
	upstream := &recordingUpstream{bodies: make(chan []byte, 1)}
	gw := startGateway(t, map[string]http.Handler{"mock-model": upstream})

	gw.post(t, t.Context(), `{"model":"mock-model","max_tokens":50,`+helloMessages+`}`)

	require.Equal(t, 50, maxTokensIn(t, <-upstream.bodies))
	require.Equal(t, 50, gw.biller.nextReservation(t).MaxOutputTokens)
}

// 预扣的金额要原样带到结算，结算才知道该退多少。
func TestSettleCarriesTheReservedAmount(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{"mock-model": mockupstream.New(mockupstream.Options{Tokens: 3})})
	gw.biller.reserved.Store(777)

	gw.post(t, t.Context(), `{"model":"mock-model",`+helloMessages+`}`)

	require.EqualValues(t, 777, gw.biller.next(t).ReservedMicro)
}

func TestRoutesEachModelToItsUpstreamWithUpstreamKey(t *testing.T) {
	a := mockupstream.New(mockupstream.Options{Tokens: 1})
	b := mockupstream.New(mockupstream.Options{Tokens: 1})
	gw := startGateway(t, map[string]http.Handler{"model-a": a, "model-b": b})

	req := gw.request(t, t.Context(), `{"model":"model-b",`+helloMessages+`}`)
	req.Header.Set("Authorization", "Bearer sk-caller-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Zero(t, a.Requests())
	require.EqualValues(t, 1, b.Requests())
	require.Equal(t, "Bearer key-model-b", b.LastHeader().Get("Authorization")) // 发给上游的只有上游密钥
}

func TestRejectsBadRequestsWithoutCallingUpstream(t *testing.T) {
	mock := mockupstream.New(mockupstream.Options{Tokens: 1})
	gw := startGateway(t, map[string]http.Handler{"mock-model": mock})

	for _, tc := range []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"invalid json", `{"model":`, http.StatusBadRequest, "invalid_request"},
		{"missing model", `{` + helloMessages + `}`, http.StatusBadRequest, "invalid_request"},
		{"unknown model", `{"model":"no-such-model",` + helloMessages + `}`, http.StatusNotFound, "model_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := gw.post(t, t.Context(), tc.body)
			require.Equal(t, tc.status, resp.StatusCode)
			require.Equal(t, tc.code, errorIn(t, resp).Code)
		})
	}
	require.Zero(t, mock.Requests())
}

func TestModelsListsConfiguredModels(t *testing.T) {
	gw := startGateway(t, map[string]http.Handler{
		"model-a": mockupstream.New(mockupstream.Options{}),
		"model-b": mockupstream.New(mockupstream.Options{}),
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gw.url+"/v1/models", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list openai.ModelList
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Equal(t, "list", list.Object)
	require.ElementsMatch(t, []openai.Model{
		{ID: "model-a", Object: "model", OwnedBy: "upstream-model-a"},
		{ID: "model-b", Object: "model", OwnedBy: "upstream-model-b"},
	}, list.Data)
}

// recordingUpstream 记下收到的请求体，并回一个最小的非流式响应。
type recordingUpstream struct {
	bodies chan []byte
}

func (u *recordingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.bodies <- body
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
}

// maxTokensIn 取出发给上游的请求里的输出上限。
func maxTokensIn(t *testing.T, body []byte) int {
	t.Helper()
	var sent struct {
		MaxTokens int `json:"max_tokens"`
	}
	require.NoError(t, json.Unmarshal(body, &sent))
	return sent.MaxTokens
}

// errorIn 解析调用方收到的错误响应。
func errorIn(t *testing.T, resp *http.Response) openai.Error {
	t.Helper()
	var body openai.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Error
}

// readEvents 读完调用方收到的整个 SSE 流，返回每个事件的 data。
func readEvents(t *testing.T, body io.Reader) []string {
	t.Helper()
	var events []string
	r := sse.NewReader(body)
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		require.NoError(t, err)
		events = append(events, string(ev.Data))
	}
}

// chunksIn 把 [DONE] 之外的事件解析成流式响应。
func chunksIn(t *testing.T, events []string) []openai.ChatResponse {
	t.Helper()
	var chunks []openai.ChatResponse
	for _, data := range events {
		if data == "[DONE]" {
			continue
		}
		var chunk openai.ChatResponse
		require.NoError(t, json.Unmarshal([]byte(data), &chunk), data)
		chunks = append(chunks, chunk)
	}
	return chunks
}

// answerIn 把流里各个事件的增量拼成完整的回答。
func answerIn(t *testing.T, events []string) string {
	t.Helper()
	var answer strings.Builder
	for _, chunk := range chunksIn(t, events) {
		for _, choice := range chunk.Choices {
			answer.WriteString(string(choice.Delta.Content))
		}
	}
	return answer.String()
}

// usageEventsIn 数出流里带 usage 的事件。
func usageEventsIn(t *testing.T, events []string) int {
	t.Helper()
	n := 0
	for _, chunk := range chunksIn(t, events) {
		if chunk.Usage != nil {
			n++
		}
	}
	return n
}

// recordingBiller 记下每次预扣和结算收到的内容。reserveOK 置为 false 就是余额不够。
type recordingBiller struct {
	reservations chan relay.Reservation
	results      chan relay.Result
	reserveOK    atomic.Bool
	reserved     atomic.Int64
}

func (b *recordingBiller) Reserve(_ context.Context, r relay.Reservation) (int64, bool, error) {
	b.reservations <- r
	return b.reserved.Load(), b.reserveOK.Load(), nil
}

func (b *recordingBiller) Settle(_ context.Context, r relay.Result) error {
	b.results <- r
	return nil
}

// next 等待下一次结算。结算发生在转发结束之后；调用方中途断开时，还要等网关察觉断开。
func (b *recordingBiller) next(t *testing.T) relay.Result {
	t.Helper()
	select {
	case r := <-b.results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没有发生结算")
		return relay.Result{}
	}
}

// nextReservation 取出这次请求的预扣信息。
func (b *recordingBiller) nextReservation(t *testing.T) relay.Reservation {
	t.Helper()
	select {
	case r := <-b.reservations:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没有发生预扣")
		return relay.Reservation{}
	}
}

type gateway struct {
	url    string
	biller *recordingBiller
}

// startGateway 起一个网关。models 把模型名映射到服务它的上游（通常是 mock），每个 handler 单独算一个上游。
func startGateway(t *testing.T, models map[string]http.Handler) *gateway {
	t.Helper()
	cfg := &config.Config{}
	for model, handler := range models {
		upstream := httptest.NewServer(handler)
		t.Cleanup(upstream.Close)
		name := "upstream-" + model
		cfg.Upstreams = append(cfg.Upstreams, config.Upstream{Name: name, BaseURL: upstream.URL + "/v1", Key: "key-" + model})
		cfg.Models = append(cfg.Models, config.Model{Name: model, Upstream: name, DefaultMaxTokens: defaultMaxTokens})
	}
	logger := slog.New(slog.DiscardHandler)
	biller := &recordingBiller{
		reservations: make(chan relay.Reservation, 10),
		results:      make(chan relay.Result, 10),
	}
	biller.reserveOK.Store(true)
	srv := httptest.NewServer(server.New(logger, relay.New(cfg, biller, logger), stubAuth))
	t.Cleanup(srv.Close)
	return &gateway{url: srv.URL, biller: biller}
}

// stubAuth 顶替鉴权中间件：relay 的测试不连数据库，只要 context 里有一个 Key ID 就行。
func stubAuth(c *gin.Context) {
	c.Request = c.Request.WithContext(apikey.NewContext(c.Request.Context(), 1))
	c.Next()
}

// post 以调用方的身份发一个聊天请求；取消 ctx 就是调用方中途断开。
func (g *gateway) post(t *testing.T, ctx context.Context, body string) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(g.request(t, ctx, body))
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (g *gateway) request(t *testing.T, ctx context.Context, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}
