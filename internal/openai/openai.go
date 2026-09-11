// Package openai 定义网关用到的 OpenAI Chat Completions 协议子集。
// 请求和响应只声明路由、用量采集和估算要读的字段，其余字段由 relay 原样转发；
// 另外定义统一的错误响应格式。
package openai

import (
	"encoding/json"
	"strings"
)

// 网关自己生成的错误类型。
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeUpstream       = "upstream_error"
	TypeServer         = "server_error"
)

// ErrorResponse 是统一的错误响应体：{"error": {"message", "type", "code"}}。
type ErrorResponse struct {
	Error Error `json:"error"`
}

// Error 是错误响应体里的 error 对象。
type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// NewError 构造一个错误响应体。
func NewError(typ, code, message string) ErrorResponse {
	return ErrorResponse{Error: Error{Message: message, Type: typ, Code: code}}
}

// ChatRequest 是请求体里网关要读的字段。
type ChatRequest struct {
	Model         string         `json:"model"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options"`
	Messages      []Message      `json:"messages"`
}

// StreamOptions 是流式请求的选项。IncludeUsage 为 true 时，上游在流的末尾多发一个只含 usage 的事件。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// IncludeUsage 表示请求是否要求在流里报告用量。
func (r ChatRequest) IncludeUsage() bool {
	return r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}

// StreamDone 是流式响应最后一个事件的 data，表示流正常结束。
const StreamDone = "[DONE]"

// ChatResponse 是非流式响应和流式事件里网关要读的字段。
type ChatResponse struct {
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage"`
}

// Choice 是一个候选回答：非流式响应的内容在 Message 里，流式事件的增量在 Delta 里。
type Choice struct {
	Message Message `json:"message"`
	Delta   Message `json:"delta"`
}

// Message 是请求里的消息、响应里的 message 和 delta 共用的结构，只含估算用量要数的文本字段。
type Message struct {
	Content          Content    `json:"content"`
	ReasoningContent string     `json:"reasoning_content"` // DeepSeek 推理模型的思考过程，按输出计费
	ToolCalls        []ToolCall `json:"tool_calls"`
}

// ToolCall 是模型发起的工具调用，参数文本同样计入用量。
type ToolCall struct {
	Function struct {
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Usage 是一次请求的用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Content 是消息内容里的文本。协议里 content 可以是字符串、分段数组或 null，这里统一拼成一个字符串：
// 图片等没有文本的分段被忽略，认不出的形状当作没有文本，留给上游去校验。
type Content string

// UnmarshalJSON 实现 json.Unmarshaler。
func (c *Content) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = Content(s)
		return nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		*c = Content(b.String())
	}
	return nil
}

// ModelList 是 GET /v1/models 的响应体。
type ModelList struct {
	Object string  `json:"object"` // 固定为 "list"
	Data   []Model `json:"data"`
}

// Model 是模型列表里的一项。
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`  // 固定为 "model"
	Created int64  `json:"created"` // 配置里没有创建时间，填 0
	OwnedBy string `json:"owned_by"`
}
