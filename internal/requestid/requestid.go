// Package requestid 为每个请求生成 ID：写进响应头 X-Request-ID，并放进请求的 context，供日志和结算使用。
package requestid

import (
	"context"
	"crypto/rand"
	"log/slog"

	"github.com/gin-gonic/gin"
)

// Header 是携带请求 ID 的响应头。
const Header = "X-Request-ID"

type ctxKey struct{}

// Middleware 为每个请求生成一个新的 ID。调用方传来的 X-Request-ID 不采用：
// 请求 ID 是用量记录的唯一键，必须由网关保证唯一。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := rand.Text()
		c.Header(Header, id)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxKey{}, id))
		c.Next()
	}
}

// From 取出 ctx 里的请求 ID，没有时返回空字符串。
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// LogHandler 包装一个 slog.Handler：用 InfoContext 这类带 context 的方法写日志时，自动加上 request_id 字段。
func LogHandler(next slog.Handler) slog.Handler {
	return logHandler{next}
}

type logHandler struct {
	slog.Handler
}

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := From(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs 和 WithGroup 也要再包一层，否则 logger.With(...) 派生出的 logger 会丢掉 request_id。
func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{h.Handler.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{h.Handler.WithGroup(name)}
}
