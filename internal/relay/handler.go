// Package relay 把调用方的 Chat Completions 请求按模型转发给对应的上游，把响应写回调用方，
// 并采集这次请求的用量：上游报告了就用上游的，否则由网关估算。转发结束后把用量交给 Biller 结算。
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/requestid"
	"github.com/eeee0717/llm-gateway/internal/sse"
)

const (
	// maxRequestBytes 是请求体的大小上限。消息里可能带 base64 编码的图片，所以给得比较宽。
	maxRequestBytes = 20 << 20
	// maxErrorBytes 是读取上游错误响应体的上限。正常的错误响应只有几百字节。
	maxErrorBytes = 64 << 10
)

// Reservation 是预扣所需的信息：一次请求最多可能花多少钱，由它算出来。
type Reservation struct {
	Model           string
	PromptTokens    int // 估算的 prompt token
	MaxOutputTokens int // 这次请求的输出上限
}

// Result 是一次转发的结果，交给 Biller 结算。
type Result struct {
	RequestID     string
	Model         string
	Usage         openai.Usage
	Estimated     bool  // 用量是网关估算的，而不是上游报告的
	ReservedMicro int64 // 这次请求预扣的金额，结算时多退少补
}

// Biller 是 relay 对计费的全部要求。接口由 relay 定义，实现由 cmd/gateway 注入。
type Biller interface {
	// Reserve 在转发之前预扣最大可能的费用，返回预扣的金额。
	// ok 为 false 表示余额不够或者 Key 被禁用，这次请求不该转发；err 只表示系统故障，例如数据库连不上。
	Reserve(ctx context.Context, r Reservation) (reserved int64, ok bool, err error)
	// Settle 结算一次请求。调用方可能已经断开，传进来的 ctx 不会随请求取消。
	Settle(ctx context.Context, r Result) error
}

// Handler 处理 /v1/chat/completions 和 /v1/models。
type Handler struct {
	routes map[string]route // 模型名 → 转发目标
	models openai.ModelList // GET /v1/models 的响应，启动时按配置生成
	client *http.Client
	biller Biller
	logger *slog.Logger
}

// New 按配置建立模型到上游的路由表。cfg 应当已经通过 config.Load 的校验。
func New(cfg *config.Config, biller Biller, logger *slog.Logger) *Handler {
	upstreams := make(map[string]config.Upstream, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		upstreams[u.Name] = u
	}
	h := &Handler{
		routes: make(map[string]route, len(cfg.Models)),
		models: openai.ModelList{Object: "list"},
		client: newClient(),
		biller: biller,
		logger: logger,
	}
	for _, m := range cfg.Models {
		u := upstreams[m.Upstream]
		h.routes[m.Name] = route{
			upstream:         u.Name,
			url:              strings.TrimSuffix(u.BaseURL, "/") + "/chat/completions",
			key:              u.Key,
			defaultMaxTokens: m.DefaultMaxTokens,
		}
		h.models.Data = append(h.models.Data, openai.Model{ID: m.Name, Object: "model", OwnedBy: u.Name})
	}
	return h
}

// Models 处理 GET /v1/models，列出配置里的全部模型。
func (h *Handler) Models(c *gin.Context) {
	c.JSON(http.StatusOK, h.models)
}

// ChatCompletions 处理 POST /v1/chat/completions。
func (h *Handler) ChatCompletions(c *gin.Context) {
	body, req, err := readRequest(c)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(openai.TypeInvalidRequest, "invalid_request", err.Error()))
		return
	}
	rt, ok := h.routes[req.Model]
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, openai.NewError(openai.TypeInvalidRequest, "model_not_found", fmt.Sprintf("model %q does not exist", req.Model)))
		return
	}
	maxOutput := req.OutputLimit()
	if maxOutput == 0 {
		maxOutput = rt.defaultMaxTokens
	}
	if body, err = prepareBody(body, req, maxOutput); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(openai.TypeInvalidRequest, "invalid_request", err.Error()))
		return
	}

	ctx := c.Request.Context()
	m := newMeter(req.Messages)
	reserved, ok, err := h.biller.Reserve(ctx, Reservation{
		Model:           req.Model,
		PromptTokens:    m.promptTokens(),
		MaxOutputTokens: maxOutput,
	})
	switch {
	case err != nil:
		h.logger.ErrorContext(ctx, "reserve failed", "error", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(openai.TypeServer, "internal_error", "internal server error"))
		return
	case !ok:
		c.AbortWithStatusJSON(http.StatusTooManyRequests,
			openai.NewError(openai.TypeInvalidRequest, "insufficient_quota", "insufficient balance for this request"))
		return
	}

	upstreamFailed := false
	// 预扣已经发生，结算必须跟上，所以放进 defer：转发过程中即使 panic，预扣也会被退回
	defer func() {
		usage, estimated := m.usage(upstreamFailed)
		result := Result{
			RequestID:     requestid.From(ctx),
			Model:         req.Model,
			Usage:         usage,
			Estimated:     estimated,
			ReservedMicro: reserved,
		}
		// 调用方可能已经断开，结算不能随请求一起取消
		if err := h.biller.Settle(context.WithoutCancel(ctx), result); err != nil {
			h.logger.ErrorContext(ctx, "settle failed", "error", err)
		}
	}()
	upstreamFailed = h.forward(c, rt, body, req, m)
}

// readRequest 读出请求体，并解析网关要用的字段。
func readRequest(c *gin.Context) ([]byte, openai.ChatRequest, error) {
	var req openai.ChatRequest
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBytes))
	if err != nil {
		return nil, req, fmt.Errorf("read request body: %w", err)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, req, fmt.Errorf("parse request body: %w", err)
	}
	if req.Model == "" {
		return nil, req, errors.New("model is required")
	}
	return body, req, nil
}

// forward 把请求发给上游，再把上游的响应写回调用方。返回值表示上游是否出错，调用方中途断开不算。
func (h *Handler) forward(c *gin.Context, rt route, body []byte, req openai.ChatRequest, m *meter) (upstreamFailed bool) {
	ctx := c.Request.Context()
	resp, err := h.send(ctx, rt, body)
	if err != nil {
		if ctx.Err() != nil {
			return false // 调用方中途断开，上游请求随之取消
		}
		h.logger.WarnContext(ctx, "upstream request failed", "upstream", rt.upstream, "error", err)
		c.AbortWithStatusJSON(http.StatusBadGateway, openai.NewError(openai.TypeUpstream, "upstream_error", "upstream request failed"))
		return true
	}
	defer resp.Body.Close()
	// 有的上游出错时照样回 200，只是响应体换成了错误；流式请求收到这种响应也按上游出错处理
	notStream := req.Stream && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if resp.StatusCode != http.StatusOK || notStream {
		h.writeUpstreamError(c, rt, resp)
		return true
	}
	if req.Stream {
		return h.relayStream(c, resp.Body, req.IncludeUsage(), m)
	}
	return h.relayJSON(c, resp, m)
}

// writeUpstreamError 把上游的错误响应转成统一格式写回调用方，状态码保持不变。有两个例外：
// 401/403 说明上游密钥有问题，而原始响应体里可能带着密钥片段，一律换成 502 upstream_auth_failed；
// 200（流式请求收到了非 SSE 的响应）换成 502，否则调用方会以为请求成功了。
func (h *Handler) writeUpstreamError(c *gin.Context, rt route, resp *http.Response) {
	h.logger.WarnContext(c.Request.Context(), "upstream returned error",
		"upstream", rt.upstream, "status", resp.StatusCode, "content_type", resp.Header.Get("Content-Type"))
	status := resp.StatusCode
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		c.AbortWithStatusJSON(http.StatusBadGateway, openai.NewError(openai.TypeUpstream, "upstream_auth_failed", "upstream rejected the gateway's credentials"))
		return
	case http.StatusOK:
		status = http.StatusBadGateway
	}
	// 读不全时下面的解析会失败，走兜底的错误信息
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	var body openai.ErrorResponse
	// 字段类型对不上（比如 code 是数字）时 Unmarshal 会报错，但其余字段照样填好，所以只看 message 有没有解析出来
	_ = json.Unmarshal(raw, &body)
	if body.Error.Message == "" {
		body = openai.NewError(openai.TypeUpstream, "upstream_error", fmt.Sprintf("upstream returned an unexpected response (status %d)", resp.StatusCode))
	}
	c.AbortWithStatusJSON(status, body)
}

// relayJSON 把非流式响应原样写回调用方。
func (h *Handler) relayJSON(c *gin.Context, resp *http.Response, m *meter) (upstreamFailed bool) {
	ctx := c.Request.Context()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return false // 调用方中途断开
		}
		h.logger.WarnContext(ctx, "upstream response interrupted", "error", err)
		c.AbortWithStatusJSON(http.StatusBadGateway, openai.NewError(openai.TypeUpstream, "upstream_interrupted", "upstream response interrupted"))
		return true
	}
	var r openai.ChatResponse
	if err := json.Unmarshal(body, &r); err == nil {
		m.observe(r)
	}
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	return false
}

// relayStream 逐个事件把上游的 SSE 流转发给调用方，每写完一个事件就刷新，让调用方立刻收到。
func (h *Handler) relayStream(c *gin.Context, body io.Reader, includeUsage bool, m *meter) (upstreamFailed bool) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	c.Writer.Flush() // 上游已经接受请求，先把响应头发给调用方

	events := sse.NewReader(body)
	done := false
	for {
		ev, err := events.Next()
		if err != nil {
			if done || c.Request.Context().Err() != nil {
				return false // 正常结束，或者调用方中途断开
			}
			// 没等到 [DONE] 流就断了。响应头早已发出，只能补一个错误事件，让调用方知道回答被截断
			h.logger.WarnContext(c.Request.Context(), "upstream stream interrupted", "error", err)
			writeErrorEvent(c, "upstream_interrupted", "upstream stream interrupted")
			return true
		}

		switch {
		case string(ev.Data) == openai.StreamDone:
			done = true // 不马上返回，继续读到 EOF：响应体读完，连接才能复用
		case len(ev.Data) > 0:
			var chunk openai.ChatResponse
			if err := json.Unmarshal(ev.Data, &chunk); err == nil {
				m.observe(chunk)
				// 只含 usage 的事件是网关替调用方要来的，调用方没要就不转发
				if !includeUsage && chunk.Usage != nil && len(chunk.Choices) == 0 {
					continue
				}
			}
		}
		if _, err := c.Writer.Write(ev.Raw); err != nil {
			return false // 调用方中途断开
		}
		c.Writer.Flush()
	}
}

// writeErrorEvent 在已经开始的 SSE 流里写一个错误事件。OpenAI 官方 SDK 读到它会抛异常。
func writeErrorEvent(c *gin.Context, code, message string) {
	data, _ := json.Marshal(openai.NewError(openai.TypeUpstream, code, message))
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}
