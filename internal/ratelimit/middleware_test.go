package ratelimit_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/ratelimit"
)

// 超过额度的请求被拒，并告诉调用方等多久。额度 1 的桶，下一个令牌在 60 秒后。
func TestMiddlewareRejectsRequestsOverTheQuota(t *testing.T) {
	l, _ := newLimiter(t)
	url := startLimited(t, l, caller(rand.Int64(), 1))

	require.Equal(t, http.StatusOK, get(t, url).StatusCode)
	resp := get(t, url)

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "60", resp.Header.Get("Retry-After"))
	require.Equal(t, "rate_limit_exceeded", errorCodeIn(t, resp))
}

// 没有经过鉴权的请求不限流：没有 Key 就没有桶，让后面的处理去拒绝它。
func TestMiddlewarePassesRequestsWithoutACaller(t *testing.T) {
	l, _ := newLimiter(t)
	none := func(context.Context) (int64, int, bool) { return 0, 0, false }
	url := startLimited(t, l, none)

	for range 5 {
		require.Equal(t, http.StatusOK, get(t, url).StatusCode)
	}
}

// caller 固定返回同一个调用方，省得在中间件测试里再搭一套鉴权。
func caller(keyID int64, rpm int) ratelimit.Caller {
	return func(context.Context) (int64, int, bool) { return keyID, rpm, true }
}

func startLimited(t *testing.T, l *ratelimit.Limiter, c ratelimit.Caller) string {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(ratelimit.Middleware(l, c))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func errorCodeIn(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body openai.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Error.Code
}
