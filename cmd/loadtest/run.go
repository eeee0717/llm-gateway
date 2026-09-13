package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/eeee0717/llm-gateway/internal/sse"
)

// target 是被压的一端：直连上游，或者经过网关。两端的请求体一模一样。
type target struct {
	Name string // 报告里的列名
	URL  string // 到 /v1 为止的地址，后面拼 /chat/completions
	Key  string
}

// firstEventLatency 打一次流式请求，返回从发出请求到收到第一个 SSE 事件的时间。
//
// 计时到"第一个事件"为止，而不是整段读完：网关的开销全部发生在转发的路上，
// 之后的事件只是沿着同一条已经建好的流走，把它们算进来只会稀释要看的那个数。
func firstEventLatency(ctx context.Context, c *http.Client, t target, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Key != "" {
		req.Header.Set("Authorization", "Bearer "+t.Key)
	}

	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	// 读完剩下的流再关，连接才回得了连接池；半路 Close 会让下一次请求重新握手，
	// 测出来的就成了建连时间。
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	event, err := sse.NewReader(resp.Body).Next()
	if err != nil {
		return 0, fmt.Errorf("read first event: %w", err)
	}
	elapsed := time.Since(start)
	if len(event.Data) == 0 {
		return 0, errors.New("first event carried no data")
	}
	return elapsed, nil
}
