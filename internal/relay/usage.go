package relay

import (
	"unicode/utf8"

	"github.com/eeee0717/llm-gateway/internal/openai"
)

// meter 采集一次请求的用量。上游报告了就用上游的；没报告就按字符数估算：
// prompt token 按请求里的消息，completion token 按已经转发给调用方的回答。
type meter struct {
	reported   *openai.Usage
	prompt     charCount
	completion charCount
}

func newMeter(messages []openai.Message) *meter {
	m := &meter{}
	for _, msg := range messages {
		m.prompt.addMessage(msg)
	}
	return m
}

// promptTokens 返回估算出来的 prompt token 数，预扣时用。
func (m *meter) promptTokens() int {
	return m.prompt.tokens()
}

// observe 记下一段转发给调用方的响应：非流式的完整响应，或者流式的一个事件。
func (m *meter) observe(r openai.ChatResponse) {
	if r.Usage != nil {
		m.reported = r.Usage
	}
	for _, choice := range r.Choices {
		m.completion.addMessage(choice.Message)
		m.completion.addMessage(choice.Delta)
	}
}

// usage 返回这次请求的用量，以及它是不是估算的。
// 上游报告了用量就用上游的；上游出错、而调用方什么也没收到时记零用量；其余情况按估算。
func (m *meter) usage(upstreamFailed bool) (openai.Usage, bool) {
	switch {
	case m.reported != nil:
		return *m.reported, false
	case upstreamFailed && m.completion.empty():
		return openai.Usage{}, true
	default:
		return openai.Usage{PromptTokens: m.prompt.tokens(), CompletionTokens: m.completion.tokens()}, true
	}
}

// charCount 分别统计 ASCII 字符和其他字符的个数，用来估算 token 数。
type charCount struct {
	ascii, other int
}

func (c charCount) empty() bool {
	return c.ascii == 0 && c.other == 0
}

func (c *charCount) addMessage(msg openai.Message) {
	c.add(string(msg.Content))
	c.add(msg.ReasoningContent)
	for _, call := range msg.ToolCalls {
		c.add(call.Function.Arguments)
	}
}

func (c *charCount) add(s string) {
	for _, r := range s {
		if r < utf8.RuneSelf {
			c.ascii++
		} else {
			c.other++
		}
	}
}

// tokens 按 DeepSeek 文档给的比例估算 token 数：1 个英文字符约 0.3 个 token，1 个中文字符约 0.6 个 token。
// ASCII 字符都按英文算，其余字符都按中文算，结果向上取整。这组系数有待对照真实上游校准。
func (c charCount) tokens() int {
	return (3*c.ascii + 6*c.other + 9) / 10
}
