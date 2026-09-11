// Package mockupstream 实现一个行为确定的 OpenAI 兼容上游，供测试、手动调试和压测使用。
//
// 回答由 Options.Tokens 个 token 组成，依次取自一句固定的英文；prompt token 数等于请求里所有消息文本的字符数。
// 同样的请求总是得到同样的回答和用量。
package mockupstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/eeee0717/llm-gateway/internal/openai"
)

// Options 控制 mock 的行为。
type Options struct {
	Tokens     int           // 回答的 token 数，流式时每个 token 一个事件
	FirstDelay time.Duration // 第一个 token 之前的等待，模拟首 token 延迟
	TokenDelay time.Duration // 相邻两个 token 之间的等待
	FailStatus int           // 非 0 时不生成回答，直接返回这个状态码和一个 OpenAI 格式的错误
	// AbortAfter 非 0 时模拟上游中途出错：流式输出这么多个 token 后直接断开连接，非流式写出一半响应体就断开。
	AbortAfter int
}

// Server 是 mock 上游，实现 http.Handler。
type Server struct {
	opts     Options
	requests atomic.Int64
	canceled atomic.Int64

	mu         sync.Mutex
	lastHeader http.Header
}

// New 返回一个按 opts 行事的 mock 上游。
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// Requests 返回收到的聊天请求数。
func (s *Server) Requests() int64 {
	return s.requests.Load()
}

// Canceled 返回回答完成之前就被对方取消的请求数。
// 取消只在等待期间（首 token 之前、token 之间）检测得到，所以观察取消的测试要给 mock 设上延迟。
func (s *Server) Canceled() int64 {
	return s.canceled.Load()
}

// LastHeader 返回最近一个聊天请求的请求头。
func (s *Server) LastHeader() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHeader
}

// words 是回答的内容，每个元素算一个 token，不够时从头循环。
var words = []string{"The", " quick", " brown", " fox", " jumps", " over", " the", " lazy", " dog", "."}

func token(i int) string {
	return words[i%len(words)]
}

// created 是响应里固定的创建时间，保证输出确定。
const created = 1700000000

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	s.requests.Add(1)
	s.mu.Lock()
	s.lastHeader = r.Header.Clone()
	s.mu.Unlock()

	var req openai.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest)
		return
	}
	if s.opts.FailStatus != 0 {
		writeError(w, s.opts.FailStatus)
		return
	}
	if req.Stream {
		s.stream(r.Context(), w, req)
		return
	}
	s.complete(r.Context(), w, req)
}

// complete 返回非流式响应。和真实上游一样，要等整个回答生成完才返回。
func (s *Server) complete(ctx context.Context, w http.ResponseWriter, req openai.ChatRequest) {
	if !s.wait(ctx, s.opts.FirstDelay+time.Duration(max(s.opts.Tokens-1, 0))*s.opts.TokenDelay) {
		return
	}
	var answer strings.Builder
	for i := range s.opts.Tokens {
		answer.WriteString(token(i))
	}
	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": created,
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer.String()},
			"finish_reason": "stop",
		}},
		"usage": usage(req, s.opts.Tokens),
	}
	if s.opts.AbortAfter > 0 {
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:len(data)/2])
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler) // net/http 约定的中止方式：不写结尾，直接断开连接
	}
	writeJSON(w, http.StatusOK, resp)
}

// stream 返回流式响应：每个 token 一个事件，接着是带 finish_reason 的事件；
// 请求要求 include_usage 时再发一个只含 usage 的事件；最后是 [DONE]。
func (s *Server) stream(ctx context.Context, w http.ResponseWriter, req openai.ChatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for i := range s.opts.Tokens {
		delay := s.opts.TokenDelay
		if i == 0 {
			delay = s.opts.FirstDelay
		}
		if !s.wait(ctx, delay) {
			return
		}
		writeEvent(w, chunk(req, choice(map[string]any{"content": token(i)}, nil)))
		_ = rc.Flush()
		if i+1 == s.opts.AbortAfter {
			panic(http.ErrAbortHandler)
		}
	}
	writeEvent(w, chunk(req, choice(map[string]any{}, "stop")))
	if req.IncludeUsage() {
		last := chunk(req)
		last["usage"] = usage(req, s.opts.Tokens)
		writeEvent(w, last)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", openai.StreamDone)
}

// wait 等待 d。对方在此期间取消了请求，就记一次取消并返回 false。
func (s *Server) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		s.canceled.Add(1)
		return false
	case <-time.After(d):
		return true
	}
}

// chunk 返回一个流式事件。请求要求 include_usage 时，和 OpenAI 一样在每个事件里带上 "usage": null。
func chunk(req openai.ChatRequest, choices ...any) map[string]any {
	c := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   req.Model,
		"choices": append([]any{}, choices...),
	}
	if req.IncludeUsage() {
		c["usage"] = nil
	}
	return c
}

func choice(delta map[string]any, finishReason any) map[string]any {
	return map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
}

// usage 是 mock 报告的用量：prompt token 数等于所有消息文本的字符数，completion token 数等于回答的 token 数。
func usage(req openai.ChatRequest, completion int) map[string]int {
	prompt := 0
	for _, m := range req.Messages {
		prompt += utf8.RuneCountInString(string(m.Content))
	}
	return map[string]int{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
}

func writeEvent(w http.ResponseWriter, v any) {
	data, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeError(w http.ResponseWriter, status int) {
	writeJSON(w, status, openai.NewError("mock_error", "mock_failure", fmt.Sprintf("mock upstream error (status %d)", status)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
