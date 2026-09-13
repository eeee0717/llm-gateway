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
	Name string   // 报告里的列名
	URL  string   // 到 /v1 为止的地址，后面拼 /chat/completions
	Keys []string // 轮流用；为空时不带 Authorization
}

// keyFor 按序号挑一个 Key。多个 Key 轮着用，免得所有请求的预扣都挤在同一行上。
func (t target) keyFor(i int) string {
	if len(t.Keys) == 0 {
		return ""
	}
	return t.Keys[i%len(t.Keys)]
}

// timing 是一次请求的两个时间点。
type timing struct {
	first time.Duration // 第一个 SSE 事件到达；延迟对照看这个
	total time.Duration // 整个流读完；吞吐看这个
}

// request 打一次流式请求，读完整个流。
//
// 延迟对照只取到"第一个事件"为止：网关的开销全部发生在转发的路上，
// 之后的事件只是沿着同一条已经建好的流走，把它们算进来只会稀释要看的那个数。
func request(ctx context.Context, c *http.Client, url, key string, body []byte) (timing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return timing{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return timing{}, err
	}
	// 读完剩下的流再关，连接才回得了连接池；半路 Close 会让下一次请求重新握手，
	// 测出来的就成了建连时间。
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return timing{}, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	event, err := sse.NewReader(resp.Body).Next()
	if err != nil {
		return timing{}, fmt.Errorf("read first event: %w", err)
	}
	got := timing{first: time.Since(start)}
	if len(event.Data) == 0 {
		return timing{}, errors.New("first event carried no data")
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return timing{}, fmt.Errorf("read the rest of the stream: %w", err)
	}
	got.total = time.Since(start)
	return got, nil
}

// saturate 用固定的并发连着打一端，直到时间用完，返回每个请求从头到尾的耗时。
//
// 吞吐不像延迟对照那样两端交替：交替的意义是抵消上游随时间的漂移，而吞吐只对
// mock 上游有意义（真实上游的速率限制会先一步成为瓶颈），mock 的行为不随时间变。
// 两端分开跑才能各自打满并发。
//
// 每个 worker 固定用一个 Key。Key 比 worker 少时才会出现同一行上的预扣排队，
// 那时量的就不是网关的处理能力，而是行锁。
func saturate(ctx context.Context, c *http.Client, t target, body []byte, workers int, d time.Duration) ([]time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	var mu sync.Mutex
	var all []time.Duration
	var failed error

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []time.Duration
			defer func() {
				mu.Lock()
				all = append(all, mine...)
				mu.Unlock()
			}()
			for ctx.Err() == nil {
				got, err := request(ctx, c, t.URL, t.keyFor(w), body)
				if err != nil {
					// 时间到了，正在路上的请求被取消——那是收尾，不是故障。
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					if failed == nil {
						failed = fmt.Errorf("%s: %w", t.Name, err)
					}
					mu.Unlock()
					return
				}
				mine = append(mine, got.total)
			}
		}()
	}
	wg.Wait()
	return all, failed
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
					got, err := request(ctx, c, targets[side].URL, targets[side].keyFor(i), body)
					if err != nil {
						errs <- fmt.Errorf("%s: %w", targets[side].Name, err)
						return
					}
					results <- outcome{side, got.first}
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
