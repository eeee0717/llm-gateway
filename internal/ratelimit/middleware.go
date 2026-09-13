package ratelimit

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/openai"
)

// Caller 返回这次请求该按谁限流、每分钟额度是多少。ok 为 false 表示这个请求没有调用方，不限流。
// 接口由使用方定义：限流不认识 API Key 是怎么鉴权的，由 cmd/gateway 把两者接起来。
type Caller func(ctx context.Context) (keyID int64, rpm int, ok bool)

// Middleware 给每个请求取一个令牌，取不到就返回 429，并带上 Retry-After 告诉调用方等多久。
func Middleware(l *Limiter, caller Caller) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID, rpm, ok := caller(c.Request.Context())
		if !ok {
			c.Next()
			return
		}
		allowed, retryAfter := l.Allow(c.Request.Context(), keyID, rpm)
		if !allowed {
			c.Header("Retry-After", strconv.Itoa(RetryAfterSeconds(retryAfter)))
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				openai.NewError(openai.TypeInvalidRequest, "rate_limit_exceeded", "rate limit exceeded"))
			return
		}
		c.Next()
	}
}
