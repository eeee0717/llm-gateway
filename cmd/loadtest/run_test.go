package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/mockupstream"
)

var probe = []byte(`{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

func TestMeasuresTimeToFirstEventNotTheWholeStream(t *testing.T) {
	// 首个事件 200ms，整段要 200+10×50=700ms 以上。
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{
		Tokens: 10, FirstDelay: 200 * time.Millisecond, TokenDelay: 50 * time.Millisecond,
	}))
	defer up.Close()

	got, err := request(t.Context(), http.DefaultClient, up.URL+"/v1", "", probe)

	require.NoError(t, err)
	require.GreaterOrEqual(t, got.first, 200*time.Millisecond)
	require.Less(t, got.first, 500*time.Millisecond)
	require.Greater(t, got.total, got.first) // 整段比首个事件晚
}

// 失败的请求返回得比成功的快得多，混进样本里会把分位数拉得很好看。
func TestFailedRequestIsAnErrorNotASample(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{FailStatus: http.StatusInternalServerError}))
	defer up.Close()

	_, err := request(t.Context(), http.DefaultClient, up.URL+"/v1", "", probe)

	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
}

// 两端交替打，公平地共享同一段时间窗口。这个上游前 20 个请求快、之后突然变慢：
// 先跑完一组再跑另一组的话，先跑的那组全落在快的那一半，差值就成了上游的漂移。
func TestBothSidesShareTheSameTimeWindow(t *testing.T) {
	var seen atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delay := 20 * time.Millisecond
		if seen.Add(1) > 20 {
			delay = 200 * time.Millisecond
		}
		mockupstream.New(mockupstream.Options{Tokens: 2, FirstDelay: delay}).ServeHTTP(w, r)
	}))
	defer up.Close()
	both := [2]target{{Name: "direct", URL: up.URL + "/v1"}, {Name: "gateway", URL: up.URL + "/v1"}}

	got, err := measure(t.Context(), http.DefaultClient, both, probe, 20, 1, 0)

	require.NoError(t, err)
	require.Equal(t, 0, got.Errors)
	require.Len(t, got.Samples[0], 20)
	require.Len(t, got.Samples[1], 20)
	drift := summarize(got.Samples[1]).P50 - summarize(got.Samples[0]).P50
	require.Less(t, max(drift, -drift), 50*time.Millisecond, "两端应当各摊到一半快的和一半慢的")
}

func TestRunStopsAtTheFirstErrorInsteadOfReportingNonsense(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{FailStatus: http.StatusBadGateway}))
	defer up.Close()
	both := [2]target{{Name: "direct", URL: up.URL + "/v1"}, {Name: "gateway", URL: up.URL + "/v1"}}

	_, err := measure(t.Context(), http.DefaultClient, both, probe, 4, 2, 0)

	require.Error(t, err)
	require.Contains(t, err.Error(), "502")
}

// 收口：中间那一层加进去多少延迟，压测就该报出多少。
// 用一个固定睡 60ms 再转发的反向代理冒充网关，两端打的是同一个上游。
func TestReportsTheLatencyTheMiddleLayerAdds(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3, FirstDelay: 100 * time.Millisecond}))
	defer up.Close()
	upURL, err := url.Parse(up.URL)
	require.NoError(t, err)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		httputil.NewSingleHostReverseProxy(upURL).ServeHTTP(w, r)
	}))
	defer slow.Close()
	both := [2]target{{Name: "direct", URL: up.URL + "/v1"}, {Name: "gateway", URL: slow.URL + "/v1"}}

	got, err := measure(t.Context(), http.DefaultClient, both, probe, 30, 3, 0)

	require.NoError(t, err)
	extra := summarize(got.Samples[1]).P50 - summarize(got.Samples[0]).P50
	require.InDelta(t, float64(60*time.Millisecond), float64(extra), float64(30*time.Millisecond))
}

// 多个 Key 轮流用。压测打同一个 Key 时，预扣会在同一行上排队，
// 测出来的是行锁而不是网关的处理能力。
func TestKeysAreUsedInTurn(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		mockupstream.New(mockupstream.Options{Tokens: 2}).ServeHTTP(w, r)
	}))
	defer up.Close()
	both := [2]target{
		{Name: "direct", URL: up.URL + "/v1"},
		{Name: "gateway", URL: up.URL + "/v1", Keys: []string{"k1", "k2", "k3"}},
	}

	_, err := measure(t.Context(), http.DefaultClient, both, probe, 6, 1, 0)

	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, seen["Bearer k1"])
	require.Equal(t, 2, seen["Bearer k2"])
	require.Equal(t, 2, seen["Bearer k3"])
	require.Equal(t, 6, seen[""]) // direct 那端不带 Authorization
}

// 吞吐是打满并发之后每秒完成多少个完整请求。上游每个请求 100ms，
// 10 个 worker 各自连着打 1 秒，完成数应当在 10/0.1=100 上下。
func TestThroughputSaturatesTheGivenConcurrency(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 1, FirstDelay: 100 * time.Millisecond}))
	defer up.Close()

	got, err := saturate(t.Context(), http.DefaultClient, target{URL: up.URL + "/v1"}, probe, 10, time.Second)

	require.NoError(t, err)
	require.Greater(t, len(got), 50)
	require.Less(t, len(got), 150)
}

// 时间到了之后正在路上的请求会被取消，那不是错误，不该报出来。
func TestThroughputEndingIsNotAnError(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 1, FirstDelay: 300 * time.Millisecond}))
	defer up.Close()

	got, err := saturate(t.Context(), http.DefaultClient, target{URL: up.URL + "/v1"}, probe, 4, 400*time.Millisecond)

	require.NoError(t, err)
	require.NotEmpty(t, got)
}

// Ctrl-C 打断的那一轮不能当成正常结果报出去：样本只跑了一部分，
// 而报告长得和跑完的一模一样。
func TestThroughputInterruptedIsAnError(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 1, FirstDelay: 20 * time.Millisecond}))
	defer up.Close()
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()

	_, err := saturate(ctx, http.DefaultClient, target{URL: up.URL + "/v1"}, probe, 2, time.Minute)

	require.Error(t, err)
	require.Contains(t, err.Error(), "interrupted")
}
