package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
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

// report 是一轮压测的原始样本，两组下标和 targets 对应。
type report struct {
	Targets [2]target
	Samples [2][]time.Duration
	Errors  int
}

// measure 打 n 对请求：每一对在两端各打一次。
//
// 两端交替着打，而不是先跑完一端再跑另一端。上游的延迟本来就随时间漂移，
// 分开跑的话两组经历的是不同的时间窗口，差出来的是漂移而不是网关的开销。
// 每一对内部的先后也轮换，免得固定排在后面的那端总是沾到前一次刚热好的连接。
//
// interval 大于 0 时限速：所有 worker 共用一个 ticker，每发一个请求取一次。
func measure(ctx context.Context, c *http.Client, targets [2]target, body []byte, n, workers int, interval time.Duration) (report, error) {
	// 出错时 worker 会提前退出，派生一个自己的 ctx 保证发任务的那个 goroutine 跟着收摊。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	wait := func() error {
		if tick == nil {
			return ctx.Err()
		}
		select {
		case <-tick:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	pairs := make(chan int)
	go func() {
		defer close(pairs)
		for i := range n {
			select {
			case pairs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	type outcome struct {
		side    int
		elapsed time.Duration
	}
	results := make(chan outcome, 2*n)
	errs := make(chan error, 2*n)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range pairs {
				for k := range 2 {
					side := (i + k) % 2 // 每一对轮换先后
					if err := wait(); err != nil {
						return
					}
					elapsed, err := firstEventLatency(ctx, c, targets[side], body)
					if err != nil {
						errs <- fmt.Errorf("%s: %w", targets[side].Name, err)
						return
					}
					results <- outcome{side, elapsed}
				}
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	rep := report{Targets: targets}
	for r := range results {
		rep.Samples[r.side] = append(rep.Samples[r.side], r.elapsed)
	}
	rep.Errors = len(errs)
	if err := <-errs; err != nil {
		return rep, fmt.Errorf("%d of %d requests failed, first one: %w", rep.Errors, 2*n, err)
	}
	return rep, nil
}
