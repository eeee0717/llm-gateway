package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/eeee0717/llm-gateway/internal/openai"
)

// route 是一个模型的转发目标。
type route struct {
	upstream         string // 上游名称，只用于日志
	url              string // 上游的 chat/completions 地址
	key              string // 上游密钥
	defaultMaxTokens int    // 调用方没指定输出上限时用的默认值
}

// newClient 返回访问上游用的 HTTP 客户端。
// 不设 http.Client.Timeout：它把读响应体的时间也算在内，会截断长时间的流。
func newClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 100 // 默认只有 2，并发一高就要反复新建连接
	// 非流式请求要等上游生成完整个回答才有响应头，只能设得很宽；调用方断开会更早取消请求。
	t.ResponseHeaderTimeout = 10 * time.Minute
	return &http.Client{Transport: t}
}

// send 把请求体发给上游。ctx 来自调用方的请求，调用方中途断开时上游请求随之取消。
// 请求头只带上游密钥，调用方的请求头一概不转发。
func (h *Handler) send(ctx context.Context, rt route, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rt.key)
	return h.client.Do(req)
}

// prepareBody 改写发往上游的请求体，改两处：流式请求强制要用量；调用方没指定输出上限时补上模型的默认值，
// 让上游生成的内容不会超过预扣时的假设。两处都不用改时原样返回。
// 请求体按 map 解析后再写回：其余字段的语义不变，但字段顺序、空白和转义方式可能和原文不同。
func prepareBody(body []byte, req openai.ChatRequest, maxOutputTokens int) ([]byte, error) {
	if !req.Stream && req.OutputLimit() != 0 {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if req.Stream {
		opts, err := includeUsage(fields["stream_options"])
		if err != nil {
			return nil, err
		}
		fields["stream_options"] = opts
	}
	if req.OutputLimit() == 0 {
		fields["max_tokens"] = json.RawMessage(strconv.Itoa(maxOutputTokens))
	}
	return json.Marshal(fields)
}

// includeUsage 在 stream_options 里打开 include_usage，让上游在流的末尾报告用量。
func includeUsage(raw json.RawMessage) (json.RawMessage, error) {
	var opts map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &opts); err != nil {
			return nil, err
		}
	}
	if opts == nil { // 没有 stream_options，或者它是 null
		opts = make(map[string]json.RawMessage)
	}
	opts["include_usage"] = json.RawMessage("true")
	return json.Marshal(opts)
}
