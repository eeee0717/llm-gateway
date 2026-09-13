package main

import (
	"net/http"
	"net/http/httptest"
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

	got, err := firstEventLatency(t.Context(), http.DefaultClient, target{URL: up.URL + "/v1"}, probe)

	require.NoError(t, err)
	require.GreaterOrEqual(t, got, 200*time.Millisecond)
	require.Less(t, got, 500*time.Millisecond)
}

// 失败的请求返回得比成功的快得多，混进样本里会把分位数拉得很好看。
func TestFailedRequestIsAnErrorNotASample(t *testing.T) {
	up := httptest.NewServer(mockupstream.New(mockupstream.Options{FailStatus: http.StatusInternalServerError}))
	defer up.Close()

	_, err := firstEventLatency(t.Context(), http.DefaultClient, target{URL: up.URL + "/v1"}, probe)

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
