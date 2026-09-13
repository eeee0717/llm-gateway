package main

import (
	"net/http"
	"net/http/httptest"
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
